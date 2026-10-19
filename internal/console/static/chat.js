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
        .replace(/(^|[^&\w])(PR\s+)?#(\d+)\b/g, function (all, pre, pr, n) {
          // "PR #969" names a pull request, not a card, so it stays text
          return pr ? all : pre + "<a href=\"#issue-" + n + "\" class=\"cardref\" data-issue=\"" + n + "\">#" + n + "</a>";
        });
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
  function userHTML(login, text, at, queued, images, docs) {
    // the row's images: their own names as thumbnails, the optimistic
    // object URLs until the read carries the stored ids; its documents:
    // link chips of file name and size, the same object URLs until then
    var thumbs = (images || []).map(function (im) {
      var src = im.url;
      return "<a class=\"thumb\" href=\"" + esc(src) + "\" target=\"_blank\" rel=\"noopener\"><img loading=\"lazy\" src=\"" + esc(src) + "\" alt=\"" + esc(im.name || "an image") + "\"></a>";
    }).join("");
    var chips = (docs || []).map(function (d) {
      var href = d.url ? d.url : base + "/attachments/" + d.id;
      return "<a class=\"chip\" href=\"" + esc(href) + "\" target=\"_blank\" rel=\"noopener\">" + esc(d.fileName || d.file_name) + " · " + sizeHTML(d.bytes) + "</a>";
    }).join("");
    return "<div class=\"msg you\"><span class=\"av\" aria-hidden=\"true\">" + esc(String(login).charAt(0).toUpperCase()) + "</span>" + whoHTML(login, at) +
      "<p class=\"said\">" + esc(text) + "</p>" + (thumbs ? "<div class=\"chat-thumbs\">" + thumbs + "</div>" : "") +
      (chips ? "<div class=\"chat-chips\">" + chips + "</div>" : "") +
      (queued ? "<p class=\"meta chat-queued\">queued: it goes out when the turn before it ends</p>" : "") + "</div>";
  }
  // sizeHTML is n bytes as the chips show it, the replay line's own format.
  function sizeHTML(n) {
    if (n < 1024) return n + " B";
    if (n < 1048576) return (n / 1024).toFixed(1) + " KiB";
    return (n / 1048576).toFixed(1) + " MiB";
  }
  // factoryHTML is a row factoryd wrote itself (C14): a poke or an
  // escalation by "the factory", its choices as buttons, its decider line
  // when the agent acted alone; a summary turn after /compact. A settled
  // row folds to one line — its reason, then the first words of what it
  // said — and opens on a click to the full quiet row (round 06's pick)
  function factoryHTML(m) {
    if (m.kind === "summary") {
      return "<div class=\"msg sum\"><span class=\"av\" aria-hidden=\"true\">≡</span>" + whoHTML("summary", m.at) + saidHTML(m.content) + "</div>";
    }
    var esc1 = m.kind === "escalation";
    var meta = "<p class=\"who\"><span class=\"name\">the factory</span> <time>" + stamp(m.at) + "</time>" +
      (m.repeats ? " <span class=\"rep\">" + (m.repeats + 1) + " times</span>" : "") + (esc1 && !m.settled ? " <span class=\"esc-l\">needs you</span>" : "") +
      (m.thread ? " <button type=\"button\" class=\"chip linkbtn\" data-open-thread=\"" + esc(m.thread) + "\"" +
        (m.thread_owner ? " data-thread-owner=\"" + esc(m.thread_owner) + "\"" : "") + ">thread ↗</button>" : "") + "</p>";
    var body = saidHTML(m.content);
    if (m.decider) body += "<p class=\"decider\"><span class=\"visually-hidden\">Decided by: </span>" + esc(m.decider) + "</p>";
    if (m.reason) body += "<p class=\"settled-why\"><span class=\"visually-hidden\">Settled: </span>" + esc(m.reason) + "</p>";
    if (m.choices && m.choices.length && !esc1 && !m.settled) {
      body += "<p class=\"choices\">" + m.choices.map(function (c) {
        return "<button type=\"button\" data-choice=\"" + esc(c) + "\">" + esc(c) + "</button>"; }).join(" ") + "</p>";
    }
    var fold = "";
    if (m.settled) fold = "<p class=\"fold\" role=\"button\" tabindex=\"0\" aria-expanded=\"false\" data-fold>" +
      "<span class=\"visually-hidden\">Settled: </span><span class=\"fw\">" + esc(m.reason || "seen") + "</span>" +
      "<span class=\"ft\">" + esc(m.content) + "</span></p>";
    return "<div class=\"msg agent poke" + (esc1 ? " esc" : "") + (m.settled ? " settled" : "") + "\"" +
      (m.task_id ? " data-card=\"" + esc(m.task_id) + "\"" : "") + " data-message=\"" + esc(m.id) + "\"><span class=\"av\" aria-hidden=\"true\"></span>" + meta +
      (esc1 ? "<div class=\"esc-b\">" + body + "</div>" : body) + fold + "</div>";
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
  // line, and the agent's rows between two of them as one message. A wake
  // the factory started itself is one row of its own, folded to its first
  // line (the main agent concept). A proposed call whose state in
  // proposals is pending offers its Do it as the stream's does; a used or
  // expired one says so under its block.
  function historyHTML(msgs, login, proposals) {
    var states = {};
    (proposals || []).forEach(function (p) { states[p.token] = p.state; });
    var out = [], parts = null, at, wake = null;
    function close() {
      if (wake) out.push(wakeHTML(wake));
      wake = null;
      if (parts) out.push(agentHTML(at, parts.join("")));
      parts = null;
    }
    msgs.forEach(function (m) {
      // a row the factory wrote itself renders as its own whatever its
      // role: a card thread's first row is the ask, seeded as a user row
      if (m.kind === "poke" || m.kind === "escalation" || m.kind === "summary") {
        close();
        out.push(factoryHTML(m));
        return;
      }
      if (m.role === "user") {
        close();
        if (/^wake/.test(m.birth_kind || "")) {
          wake = {text: m.content, at: m.at, parts: []};
          return;
        }
        var ims = [], docs = [];
        (m.attachments || []).forEach(function (a) { (/^image\//.test(a.content_type) ? ims : docs).push(a); });
        out.push(userHTML(login, m.content, m.at, m.queued, ims, docs));
        return;
      }
      var dst = wake ? wake.parts : parts;
      if (!dst) {
        parts = dst = [];
        at = m.at;
      }
      if (m.content) dst.push(saidHTML(m.content));
      (m.tool_calls || []).forEach(function (c) {
        var state = c.proposal && states[c.proposal];
        if (state === "pending") {
          dst.push(proposalHTML({id: c.proposal, tool: c.name, arguments: c.arguments,
            description: c.result.replace(/^Proposed to [^:]*: /, "").replace(/\. Nothing runs until they press Do it\.$/, "")}));
          return;
        }
        dst.push(toolHTML(c.name, c.arguments, null, c.result));
        if (state) dst.push("<p class=\"pfoot\"><span class=\"expired\">" + esc(state) + "</span></p>");
      });
    });
    close();
    return out.join("");
  }
  // wakeHTML is one wake turn the factory started itself (the main agent
  // concept): its rows fold to one line — wake, then the wake text's
  // first line — and open on a click to the whole turn, the settled
  // row's fold
  function wakeHTML(w) {
    var first = String(w.text || "").split("\n")[0];
    return "<div class=\"msg agent wake\"><span class=\"av\" aria-hidden=\"true\"></span>" +
      whoHTML("the factory", w.at) +
      "<div class=\"wake-b\">" + saidHTML(w.text) + w.parts.join("") + "</div>" +
      "<p class=\"fold\" role=\"button\" tabindex=\"0\" aria-expanded=\"false\" data-fold>" +
      "<span class=\"visually-hidden\">Wake: </span><span class=\"fw\">wake</span>" +
      "<span class=\"ft\">" + esc(first) + "</span></p></div>";
  }
  // threadHTML is a thread's log: the rows Clear old swept from view name
  // their count in one line at the top; after a compaction its rows up to
  // the summary fold behind one line, the summary turn follows, then the
  // rows after it (C14)
  function threadHTML(msgs, login, proposals, thread) {
    var swept = thread && thread.cleared ? "<p class=\"meta chat-cleared\">cleared: " + thread.cleared + " older " +
      (thread.cleared === 1 ? "message" : "messages") + " out of view</p>" : "";
    var upto = thread && thread.summary ? thread.summary_upto || 0 : 0;
    if (!upto) return swept + historyHTML(msgs, login, proposals);
    var old = msgs.filter(function (m) { return m.id <= upto; }), rest = msgs.filter(function (m) { return m.id > upto; });
    var at = old.length ? old[old.length - 1].at : (rest[0] || {}).at;
    return swept + (old.length ? "<details class=\"compacted\"><summary>" + old.length + " earlier " + (old.length === 1 ? "message" : "messages") +
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
  // splitOf is thread t's split as words (the settle concept's badge):
  // the asks still open need the member, the unread rows that are no open
  // ask are new
  function splitOf(t) {
    return (t.needs_you ? t.needs_you + " needs you" : "") + (t.needs_you && t.new_rows ? " · " : "") + (t.new_rows ? t.new_rows + " new" : "");
  }
  // badgeHTML is the split as pills (round 06's pick): amber for the
  // open asks, a bordered one for the new rows, either alone when only
  // its side counts
  function badgeHTML(t) {
    if (!t.needs_you && !t.new_rows) return "";
    var label = esc(splitOf(t));
    if (t.needs_you && t.new_rows) {
      return "<span class=\"bsplit\" aria-label=\"" + label + "\"><span class=\"badge\">" + esc(t.needs_you) + "</span>" +
        "<span class=\"badge new\">" + esc(t.new_rows) + "</span></span>";
    }
    var n = t.needs_you || t.new_rows;
    return "<span class=\"badge" + (t.needs_you ? "" : " new") + "\" aria-label=\"" + label + "\">" + esc(n) + "</span>";
  }
  // rowsHTML is the switcher's rows: the member's own inbox first, their
  // active threads, the other members' with their names, then the
  // archived under their rule; open is the open thread's number and owner
  function rowsHTML(list, open, openOwner, login, now) {
    var mine = function (t) { return !t.login || t.login === login; };
    var on = function (t) { return t.number === open && (t.login && t.login !== login ? t.login : "") === openOwner; };
    var active = (list || []).filter(function (t) { return t.state !== "archived"; });
    var archived = (list || []).filter(function (t) { return t.state === "archived"; });
    var rows = active.filter(mine).map(function (t) {
      var here = on(t), mark = badgeHTML(t);
      var tip = titleOf(t) + (mark ? ", " + splitOf(t) : "") + (t.working ? ", a turn in flight" : "");
      return "<button type=\"button\" class=\"trow" + (here ? " on" : "") + (mark ? " unread" : "") + (t.inbox ? " inbox" : "") +
        "\" data-thread=\"" + esc(t.number) + "\" aria-current=\"" + here + "\" title=\"" + esc(tip) + "\">" + markHTML(t) +
        "<span class=\"ttitle\">" + esc(titleOf(t)) + "</span>" + (mark ? mark : "<span class=\"tat\">" + esc(when(t.last_at, now)) + "</span>") + "</button>";
    }).join("");
    var others = active.filter(function (t) { return !mine(t); });
    rows += others.map(function (t) {
      var here = on(t);
      var tip = titleOf(t) + ", " + t.login + "'s thread" + (t.working ? ", a turn in flight" : "");
      return "<button type=\"button\" class=\"trow others" + (here ? " on" : "") +
        "\" data-thread=\"" + esc(t.number) + "\" data-owner=\"" + esc(t.login) + "\" aria-current=\"" + here + "\" title=\"" + esc(tip) + "\">" + markHTML(t) +
        "<span class=\"ttitle\">" + esc(titleOf(t)) + " <span class=\"twhose\">" + esc(t.login) + "'s</span></span>" +
        "<span class=\"tat\">" + esc(when(t.last_at, now)) + "</span></button>";
    }).join("");
    if (archived.length) {
      rows += "<div class=\"tarch\"><span class=\"tarch-l\">archived</span>" + archived.map(function (t) {
        return "<span class=\"trow arch" + (on(t) ? " on" : "") + "\"><span class=\"tmark\" aria-hidden=\"true\">arc</span>" +
          "<button type=\"button\" class=\"ttitle open-arch\" data-thread=\"" + esc(t.number) + "\"" +
          (mine(t) ? "" : " data-owner=\"" + esc(t.login) + "\"") + " aria-current=\"" + on(t) + "\">" + esc(titleOf(t)) + (mine(t) ? "" : " · " + esc(t.login) + "'s") + "</button>" +
          (mine(t) ? "<button type=\"button\" class=\"linkbtn restore\" data-restore=\"" + esc(t.number) + "\">restore</button>" : "") + "</span>";
      }).join("") + "</div>";
    }
    return rows;
  }
  // sumHTML is the folded switcher's one row on the phone, its split as
  // the badge's pills
  function sumHTML(list, login, folded) {
    var active = (list || []).filter(function (t) { return t.state !== "archived" && (!t.login || t.login === login); });
    var needs = active.reduce(function (n, t) { return n + (t.needs_you || 0); }, 0);
    var fresh = active.reduce(function (n, t) { return n + (t.new_rows || 0); }, 0);
    var sum = { needs_you: needs, new_rows: fresh };
    var mark = badgeHTML(sum);
    var working = active.filter(function (t) { return t.working; }).length;
    return "Threads <span class=\"n\">" + active.length + "</span>" + (mark ? " · " + mark : "") +
      (working ? " · <span class=\"tmark working\"><i class=\"dot\" aria-hidden=\"true\"></i></span> " + working + " working" : "") +
      "<span class=\"chev\" aria-hidden=\"true\">" + (folded ? "⌄" : "⌃") + "</span>";
  }
  // unreadOf is the tab title's count: the member's own threads' cards
  // that wait for them now, nothing when none does
  function unreadOf(list, login) {
    var own = (list || []).filter(function (t) { return !t.login || t.login === login; });
    return own.reduce(function (n, t) { return n + (t.waits_you || 0); }, 0);
  }
  // headHTML is the open thread's head: its title (a click renames it),
  // its last turn or the inbox's line or the archived note, and its acts:
  // Clear old sweeps the old rows from view on any thread with more than
  // one row, Archive and Delete… on an active thread with rows, never on
  // the inbox (C14)
  function headHTML(t, login, now) {
    var mine = !t.login || t.login === login;
    var hint = !mine ? t.login + "'s thread: read it and talk in it" :
      t.inbox ? "the factory's thread: the agent speaks here uninvited" : t.state === "archived" ? "archived, read-only" : when(t.last_at, now);
    var acts = mine && t.messages >= 2 ? "<button type=\"button\" data-clear title=\"Hide the old rows from view; the record keeps them\">Clear old</button>" : "";
    if (mine && !t.inbox && t.state !== "archived" && t.messages) {
      acts += "<button type=\"button\" data-archive>Archive</button><button type=\"button\" class=\"danger\" data-delete>Delete…</button>";
    }
    var title = esc(titleOf(t)) + (mine ? "" : " · " + esc(t.login) + "'s");
    return "<h3 class=\"ttl\">" + (t.inbox ? "<span class=\"tmark inbox\" aria-hidden=\"true\">⌂</span>" : "") +
      (t.inbox || t.state === "archived" || !mine ? title : "<button type=\"button\" class=\"rename\" data-rename title=\"Rename the thread\">" + esc(titleOf(t)) + "</button>") +
      "</h3><span class=\"thint" + (t.inbox || t.state === "archived" ? "" : " when") + "\">" + esc(hint) + "</span>" +
      (acts ? "<span class=\"tacts\">" + acts + "</span>" : "");
  }
  // pendingOf is the row the thread's answer row offers: its newest open
  // question, an escalation's to every member or the chat's own ask to
  // this one — settled, the row's reason line stands in its place (the
  // settle concept)
  function pendingOf(msgs) {
    var m = null;
    (msgs || []).forEach(function (x) { if ((x.kind === "escalation" || x.kind === "ask") && !x.settled) m = x; });
    return m && m.choices && m.choices.length ? m : null;
  }
  function answerHTML(m) {
    // an ask answers as the member's own message, an escalation through /answer
    var k = m.kind === "ask" ? "data-choice" : "data-answer";
    return "<span class=\"answer-l\">Answer:</span>" + m.choices.map(function (c) {
      return "<button type=\"button\" " + k + "=\"" + esc(c) + "\">" + esc(c) + "</button>";
    }).join("") + "<span class=\"answer-or\">or in your words below</span>";
  }
  // levelMenuHTML is the level control's menu (C14): its head, then the
  // four states with their words, the current one checked
  function levelMenuHTML(levels, cur) {
    return "<p class=\"lp-head\"><span>autonomy</span><span>a change confirms</span></p>" + levels.map(function (l) {
      var on = l.name === cur;
      return "<button type=\"button\" role=\"menuitemradio\" aria-checked=\"" + on + "\" data-lvl=\"" + esc(l.name) + "\"><i aria-hidden=\"true\"></i><b>" +
        esc(l.name) + "</b><span class=\"ck\" aria-hidden=\"true\">" + (on ? "✓" : "") + "</span><span class=\"lw\">" + esc(l.words) + "</span></button>";
    }).join("");
  }
  // levelConfirmHTML is the menu turned into the confirm of state l
  function levelConfirmHTML(l) {
    return "<div class=\"lvl-confirm\" role=\"dialog\" aria-labelledby=\"lvl-title\"><h2 id=\"lvl-title\">Set the project's autonomy to " + esc(l.name) +
      "?</h2><p>This writes a policy change and lands in the audit like every decision. From then on: " + esc(l.words) + ".</p>" +
      "<div class=\"acts\"><button type=\"button\" class=\"need-btn\" data-set=\"" + esc(l.name) + "\">Set</button><button type=\"button\" data-keep>Keep</button></div></div>";
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
  // change. An item is the text and the images and documents riding it.
  function queue(start, busy, shown) {
    var items = [];
    function flush() {
      if (items.length && !busy()) start(items.shift());
      shown(items.slice());
    }
    return {
      add: function (text, images, docs) {
        items.push({text: text, images: images || null, docs: docs || null});
        flush();
      },
      edit: function (i, text) {
        text = text.trim();
        if (text) items[i].text = text;
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
    return items.map(function (it, i) {
      var label = it.text + (it.images && it.images.length ? " · " + it.images.length + (it.images.length > 1 ? " images" : " image") : "") +
        (it.docs && it.docs.length ? " · " + it.docs.length + (it.docs.length > 1 ? " documents" : " document") : "");
      return "<li><button type=\"button\" class=\"queued-text\" data-edit=\"" + i + "\" aria-label=\"Edit queued message " + (i + 1) + ": " + esc(it.text) + "\">" + esc(label) +
        "</button><button type=\"button\" class=\"queued-remove\" data-remove=\"" + i + "\" aria-label=\"Remove queued message " + (i + 1) + "\">×</button></li>";
    }).join("");
  }
  // clampShare bounds the chat pane's width for the divider drag: the
  // chat keeps 18rem and the board 30rem of availPx.
  function clampShare(chatPx, availPx, remPx) {
    var min = 18 * remPx, max = availPx - 30 * remPx;
    return Math.max(min, Math.min(max, chatPx));
  }
  return {esc: esc, md: md, stamp: stamp, userHTML: userHTML, agentHTML: agentHTML, saidHTML: saidHTML, toolHTML: toolHTML,
    proposalHTML: proposalHTML, appliedHTML: appliedHTML, historyHTML: historyHTML, factoryHTML: factoryHTML, threadHTML: threadHTML, deleteHTML: deleteHTML, levelMenuHTML: levelMenuHTML, levelConfirmHTML: levelConfirmHTML, pendingOf: pendingOf, answerHTML: answerHTML, paletteOf: paletteOf, paletteHTML: paletteHTML, expand: expand, when: when, rowsHTML: rowsHTML,
    sumHTML: sumHTML, unreadOf: unreadOf, headHTML: headHTML, eventStream: eventStream, sse: sse, queue: queue, queueHTML: queueHTML, sizeHTML: sizeHTML, clampShare: clampShare};
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
  var threads = [], open = 0, openOwner = "", openThread = {}, running = {}, readOnly = false;
  var foldKey = "chat-fold:" + login;

  // the screen's two panes: the divider drags (or the arrow keys move) the
  // boundary, every side keeping its minimum, Full hands the whole screen
  // to the chat, Grow heights the input for a long message; the boundary
  // and Full outlive the page in localStorage
  var screenEl = document.getElementById("screen"), divider = document.getElementById("chat-divider");
  if (divider && screenEl) {
    var remPx = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
    var share = function (chatPx) {
      var avail = screenEl.clientWidth, px = chatView.clampShare(chatPx, avail, remPx);
      screenEl.style.setProperty("--chat-share", (px / Math.max(1, avail - 10 - px)).toFixed(3) + "fr");
      try { localStorage.setItem("chat-share", (px / avail).toFixed(4)); } catch (err) {}
    };
    var storedShare = 0;
    try { storedShare = parseFloat(localStorage.getItem("chat-share")) || 0; } catch (err) {}
    if (storedShare) share(storedShare * screenEl.clientWidth);
    divider.addEventListener("pointerdown", function (e) {
      e.preventDefault();
      if (divider.setPointerCapture) divider.setPointerCapture(e.pointerId);
    });
    divider.addEventListener("pointermove", function (e) {
      if (!divider.hasPointerCapture || !divider.hasPointerCapture(e.pointerId)) return;
      share(e.clientX - screenEl.getBoundingClientRect().left);
    });
    divider.addEventListener("keydown", function (e) {
      if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
      e.preventDefault();
      var step = screenEl.clientWidth * 0.03;
      var chat = screenEl.querySelector(".chat");
      share((chat ? chat.getBoundingClientRect().width : 0) + (e.key === "ArrowLeft" ? -step : step));
    });
  }
  var full = document.getElementById("chat-full");
  if (full && screenEl) {
    var setFull = function (on) {
      screenEl.classList.toggle("chat-only", on);
      full.setAttribute("aria-pressed", String(on));
      try { localStorage.setItem("chat-only", on ? "1" : "0"); } catch (err) {}
    };
    var wasFull = false;
    try { wasFull = localStorage.getItem("chat-only") === "1"; } catch (err) {}
    setFull(wasFull);
    full.addEventListener("click", function () { setFull(!screenEl.classList.contains("chat-only")); });
  }
  var grow = document.getElementById("chat-grow");
  if (grow && input) {
    grow.addEventListener("click", function () {
      var on = !input.classList.contains("tall");
      input.classList.toggle("tall", on);
      grow.setAttribute("aria-pressed", String(on));
      input.focus();
    });
  }

  // the images and documents riding the next message: pasted into the
  // input or dropped on the composer, named img1.. and doc1.. in arrival
  // order — the names the text refers to and the server keeps —
  // removable until Send, gone with the message. They live in this
  // page: a reload drops them.
  var imagesEl = document.getElementById("chat-images"), pendingImages = [];
  var docsEl = document.getElementById("chat-docs"), pendingDocs = [];
  function drawImages() {
    if (!imagesEl) return;
    imagesEl.innerHTML = pendingImages.map(function (im, i) {
      return "<li><img src=\"" + chatView.esc(im.url) + "\" alt=\"\" width=\"22\" height=\"22\"><span>" + chatView.esc(im.name) + "</span>" +
        "<button type=\"button\" class=\"queued-remove\" data-unimage=\"" + i + "\" aria-label=\"Remove " + chatView.esc(im.name) + "\">×</button></li>";
    }).join("");
    imagesEl.hidden = pendingImages.length === 0;
  }
  function addImages(files) {
    var dropped = 0;
    for (var i = 0; i < files.length; i++) {
      var f = files[i];
      if (pendingImages.length >= 4 || !f || !/^image\/(png|jpeg|gif|webp)$/.test(f.type) || f.size > 8 * 1024 * 1024) { dropped++; continue; }
      pendingImages.push({name: "img" + (pendingImages.length + 1), file: f, url: URL.createObjectURL(f)});
    }
    drawImages();
    // a drop the composer makes silently is a drop the member cannot
    // know to retry: the note names the bounds
    if (dropped) failed("Dropped " + dropped + (dropped === 1 ? " image" : " images") + ": at most 4 ride a message, each a png, jpeg, gif or webp of at most 8 MiB.");
  }
  function drawDocs() {
    if (!docsEl) return;
    docsEl.innerHTML = pendingDocs.map(function (d, i) {
      return "<li><span>" + chatView.esc(d.name + " " + d.fileName + " " + chatView.sizeHTML(d.bytes)) + "</span>" +
        "<button type=\"button\" class=\"queued-remove\" data-undoc=\"" + i + "\" aria-label=\"Remove " + chatView.esc(d.fileName) + "\">×</button></li>";
    }).join("");
    docsEl.hidden = pendingDocs.length === 0;
  }
  function addDocuments(files) {
    var dropped = 0;
    for (var i = 0; i < files.length; i++) {
      var f = files[i];
      if (pendingDocs.length >= 4 || !f) { dropped++; continue; }
      pendingDocs.push({name: "doc" + (pendingDocs.length + 1), file: f, url: URL.createObjectURL(f), fileName: f.name, bytes: f.size});
    }
    drawDocs();
    // a drop the composer makes silently is a drop the member cannot
    // know to retry: the note names the bound (kind and size stay the
    // server's words, at Send)
    if (dropped) failed("Dropped " + dropped + (dropped === 1 ? " document" : " documents") + ": at most 4 ride a message.");
  }
  // routeFiles splits files by what they are: images to the image chips,
  // everything else to the document chips — the server refuses at Send
  // what rides wrong, with its own words.
  function routeFiles(files) {
    var ims = [], docs = [];
    for (var i = 0; i < files.length; i++) {
      if (/^image\//.test(files[i].type)) ims.push(files[i]);
      else docs.push(files[i]);
    }
    if (ims.length) addImages(ims);
    if (docs.length) addDocuments(docs);
  }
  if (input) input.addEventListener("paste", function (e) {
    var files = [];
    for (var i = 0; e.clipboardData && e.clipboardData.items && i < e.clipboardData.items.length; i++) {
      var it = e.clipboardData.items[i];
      if (it.kind === "file" && it.getAsFile()) files.push(it.getAsFile());
    }
    if (files.length) {
      e.preventDefault();
      routeFiles(files);
    }
  });
  if (form) {
    form.addEventListener("dragover", function (e) { e.preventDefault(); });
    form.addEventListener("drop", function (e) {
      e.preventDefault();
      if (e.dataTransfer && e.dataTransfer.files.length) routeFiles(e.dataTransfer.files);
    });
  }
  if (imagesEl) imagesEl.addEventListener("click", function (e) {
    var b = e.target.closest("[data-unimage]");
    if (!b) return;
    pendingImages.splice(Number(b.dataset.unimage), 1);
    // the names stay dense img1..imgN, what the server hands back
    pendingImages.forEach(function (im, i) { im.name = "img" + (i + 1); });
    drawImages();
    input.focus();
  });
  if (docsEl) docsEl.addEventListener("click", function (e) {
    var b = e.target.closest("[data-undoc]");
    if (!b) return;
    pendingDocs.splice(Number(b.dataset.undoc), 1);
    // the names stay dense doc1..docN, what the server hands back
    pendingDocs.forEach(function (d, i) { d.name = "doc" + (i + 1); });
    drawDocs();
    input.focus();
  });

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
  // ref is a thread's key: its number and its owner, "" the member's own
  function ref(n, owner) { return n + "@" + (owner || ""); }
  function openRef() { return ref(open, openOwner); }
  function threadOwner(t) { return t && t.login && t.login !== login ? t.login : ""; }
  function busy(k) { return !!running[k]; }
  // lock disables the composer while the open thread is read-only and the
  // log's buttons while a turn of the open thread streams
  function lock() {
    if (!form) return;
    form.querySelectorAll("textarea, button").forEach(function (el) { el.disabled = readOnly; });
    log.setAttribute("aria-busy", String(busy(openRef())));
    log.querySelectorAll("button").forEach(function (b) { b.disabled = busy(openRef()); });
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
  function run(n, owner, url, body, into, onMissing) {
    var k = ref(n, owner);
    var ended = false, said = null, raw = "", reader = null, cursor = document.createElement("span"), wait = null;
    cursor.className = "cursor";
    cursor.setAttribute("aria-hidden", "true");
    cursor.textContent = "▍";
    into.appendChild(cursor);
    running[k] = (running[k] || 0) + 1;
    lock();
    listThreads();
    // a string body posts urlencoded; a FormData one (the send with
    // images) leaves the type to the browser, which sets the multipart
    // boundary
    var headers = {"X-CSRF-Token": form.elements.csrf.value};
    if (typeof body === "string") headers["Content-Type"] = "application/x-www-form-urlencoded";
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
      else if (ev.event === "view") document.dispatchEvent(new CustomEvent("view", {detail: d}));
      else if (ev.event === "error") add(into, "<p class=\"chat-error\">" + chatView.esc(d.message) + "</p>");
      ended = ended || ev.event === "done" || ev.event === "error";
    }
    return fetch(url, {method: "POST", body: body, headers: headers}).then(function (resp) {
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
      running[k]--;
      lock();
      queueOf(k).flush();
      return (k === openRef() ? seen(n, owner) : Promise.resolve()).then(listThreads);
    });
  }
  // seen moves thread n's seen marker to its newest row: the member read it
  function seen(n, owner) {
    return fetch(base + "/seen", {method: "POST", body: new URLSearchParams({conversation: n, owner: owner || ""}).toString(),
      headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).catch(function () {});
  }
  // each thread's own queue of messages sent while its turn streams
  var queues = {};
  function queueOf(k) {
    if (!queues[k]) {
      queues[k] = chatView.queue(function (it) { turn(k, it.text, it.images, it.docs); }, function () {
        return busy(k) || readOnly && k === openRef() || k === openRef() && !!queued.querySelector("textarea");
      }, function (items) {
        if (k !== openRef() || queued.querySelector("textarea")) return;
        queued.innerHTML = chatView.queueHTML(items);
        queued.hidden = !items.length;
      });
    }
    return queues[k];
  }
  function send() {
    var text = input.value.trim();
    if (!text || readOnly) return;
    input.value = "";
    var images = pendingImages, docs = pendingDocs;
    pendingImages = [];
    pendingDocs = [];
    drawImages();
    drawDocs();
    queueOf(openRef()).add(text, images, docs);
  }
  // pageQuery is the view the page stands on (282): the board's query the
  // chat's POST carries, so the agent's view tools work the page the
  // member looks at, not a page of their own
  function pageQuery() {
    var b = document.getElementById("board");
    return b && b.dataset.query ? b.dataset.query : "";
  }
  function turn(k, text, images, docs) {
    var at = k.indexOf("@"), n = Number(k.slice(0, at)), owner = k.slice(at + 1);
    var shown = k === openRef(), command = text === "/compact";
    if (shown) {
      pending = null;
      drawAnswer();
    }
    var into = document.createElement("div");
    if (shown) {
      if (!command) add(log, chatView.userHTML(login, chatView.expand(commands, text), new Date(), false, images, docs));
      add(log, chatView.agentHTML(new Date(), ""));
      scrollEnd(true);
      into = log.lastElementChild;
    }
    var body = new FormData();
    body.append("message", text);
    body.append("conversation", n);
    body.append("owner", owner);
    body.append("view", pageQuery());
    (images || []).forEach(function (im) { body.append("images", im.file, im.file.name); });
    (docs || []).forEach(function (d) { body.append("documents", d.file, d.file.name); });
    run(n, owner, base, body, into).then(function () {
      if (command && k === openRef()) return load(n, owner);
    }).then(function () {
      if (document.activeElement === document.body) input.focus();
    });
  }
  function expired(el) {
    return function () { el.innerHTML = "<span class=\"expired\">expired</span>"; };
  }

  // the level control under Send (C14): its menu of the four states; a
  // change confirms in place, Set writes it through the project's file,
  // and the inbox's poke follows; Escape, a click outside and Keep close
  var levelBtn = document.getElementById("level-btn"), levelPop = document.getElementById("level-pop"), levelsEl = document.getElementById("chat-levels");
  var levels = levelsEl ? JSON.parse(levelsEl.textContent || "null") || [] : [];
  function closeLevel(focus) {
    if (!levelPop || levelPop.hidden) return;
    levelPop.hidden = true;
    levelBtn.setAttribute("aria-expanded", "false");
    if (focus) levelBtn.focus();
  }
  function openLevel() {
    levelPop.innerHTML = chatView.levelMenuHTML(levels, levelBtn.dataset.level);
    levelPop.hidden = false;
    levelBtn.setAttribute("aria-expanded", "true");
    levelPop.querySelector("[aria-checked=true]").focus();
  }
  if (levelBtn && levelPop) {
    levelBtn.addEventListener("click", function () {
      if (levelPop.hidden) openLevel();
      else closeLevel(true);
    });
    levelPop.addEventListener("click", function (e) {
      // a click in the menu stays here: its content is replaced below, and
      // the outside-click close would find the clicked node gone
      e.stopPropagation();
      var pick = e.target.closest("[data-lvl]"), set = e.target.closest("[data-set]");
      if (pick && pick.dataset.lvl === levelBtn.dataset.level) closeLevel(true);
      else if (pick) {
        levelPop.innerHTML = chatView.levelConfirmHTML(levels.filter(function (l) { return l.name === pick.dataset.lvl; })[0]);
        levelPop.querySelector("[data-keep]").focus();
      } else if (set) {
        var to = set.dataset.set;
        fetch(base.replace(/\/chat$/, "/autonomy"), {method: "POST", body: new URLSearchParams({autonomy: to}).toString(),
          headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).then(function (resp) {
          if (!resp.ok) return resp.text().then(function (why) { throw new Error(why); });
          levelBtn.dataset.level = to;
          levelBtn.querySelector(".lvl-name").textContent = to;
          levelBtn.setAttribute("aria-label", "The project's autonomy level: " + to);
          closeLevel(true);
        }).catch(function (err) {
          closeLevel(true);
          failed(err);
        });
      } else if (e.target.closest("[data-keep]")) closeLevel(true);
    });
    document.addEventListener("click", function (e) {
      if (!e.target.closest(".lvlctl")) closeLevel(false);
    });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape" && !levelPop.hidden) {
        e.stopPropagation();
        closeLevel(true);
      }
    }, true);
  }
  // the intake switch in the chat's head (an approver's): a click flips
  // it — closed refuses the agent's own wake filings, open takes them
  // again, and the member's ask always files — and the button answers in
  // place
  var intakeBtn = document.getElementById("intake-btn");
  if (intakeBtn) {
    intakeBtn.addEventListener("click", function () {
      var to = intakeBtn.dataset.intake === "closed" ? "open" : "closed";
      fetch(base.replace(/\/chat$/, "/intake"), {method: "POST", body: new URLSearchParams({intake: to}).toString(),
        headers: {"Content-Type": "application/x-www-form-urlencoded", "X-CSRF-Token": form.elements.csrf.value}}).then(function (resp) {
        if (!resp.ok) return resp.text().then(function (why) { throw new Error(why); });
        intakeBtn.dataset.intake = to;
        intakeBtn.classList.toggle("closed", to === "closed");
        intakeBtn.setAttribute("aria-pressed", to === "closed" ? "true" : "false");
        var name = intakeBtn.querySelector(".intake-name");
        if (name) name.textContent = to;
      }).catch(failed);
    });
  }
  // a drawer's "From your chat thread" opens that thread here
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-open-thread]");
    var whose = b && b.dataset.threadOwner && b.dataset.threadOwner !== login ? b.dataset.threadOwner : "";
    if (b && (Number(b.dataset.openThread) !== open || whose !== openOwner)) load(Number(b.dataset.openThread), whose);
  });
  // a drawer's "Talk about this card" opens the card's own thread, made
  // on its first open (the card door)
  document.addEventListener("click", function (e) {
    var b = e.target.closest("[data-card-thread]");
    if (!b || !form) return;
    b.disabled = true;
    fetch("/cards/" + encodeURIComponent(b.dataset.cardThread) + "/thread", {method: "POST",
      headers: {"X-CSRF-Token": form.elements.csrf.value}}).then(function (resp) {
      if (!resp.ok) throw new Error(resp.status);
      return resp.json();
    }).then(function (d) {
      b.disabled = false;
      load(Number(d.conversation), d.owner && d.owner !== login ? d.owner : "");
    }).catch(function () {
      b.disabled = false;
      failed();
    });
  });
  // openCard opens a card's drawer over the board and lights the card on
  // the board, so the eye finds it where the chat named it. The scroll to
  // the card can touch off a lane's Load more, whose board swap drops the
  // light, so every board swap lights it again inside its window
  var litCard = null, litUntil = 0;
  function lightBoard(scroll) {
    if (!litCard || Date.now() > litUntil) return;
    var lit = document.querySelectorAll("#board [data-card=\"" + CSS.escape(litCard) + "\"]");
    for (var i = 0; i < lit.length; i++) {
      lit[i].classList.remove("lit");
      void lit[i].offsetWidth;
      lit[i].classList.add("lit");
      if (scroll) lit[i].scrollIntoView({block: "nearest", inline: "nearest", behavior: "smooth"});
    }
  }
  function openCard(id) {
    location.hash = "#card=" + encodeURIComponent(id);
    litCard = id;
    litUntil = Date.now() + 2600;
    lightBoard(true);
    setTimeout(function () {
      litCard = null;
      var lit = document.querySelectorAll("#board .lit");
      for (var i = 0; i < lit.length; i++) lit[i].classList.remove("lit");
    }, 2600);
  }
  document.addEventListener("htmx:afterSwap", function (e) {
    if (e.target && (e.target.id === "board" || e.target.closest("#board"))) lightBoard(false);
  });
  // #n in the model's text opens card n's drawer over the board
  document.addEventListener("click", function (e) {
    var ref = e.target.closest("a.cardref");
    if (!ref) return;
    e.preventDefault();
    var refs = document.querySelectorAll("#board [data-card] .ref");
    for (var i = 0; i < refs.length; i++) {
      if (refs[i].textContent.trim() === "#" + ref.dataset.issue) {
        openCard(refs[i].closest("[data-card]").dataset.card);
        return;
      }
    }
    var flash = document.getElementById("flash");
    flash.textContent = "#" + ref.dataset.issue + " is not on the board.";
    flash.hidden = false;
  });
  // a factory row's body opens the card it is about; its buttons, links
  // and the fold keep their own acts
  document.addEventListener("click", function (e) {
    if (e.target.closest("a, button, input, select, textarea, label, summary, details, form, [data-fold]")) return;
    var row = e.target.closest(".msg.poke[data-card]");
    if (row) openCard(row.dataset.card);
  });
  if (!form) return;
  // the queue under the input: a click on a message edits it in place
  // (Enter or leaving it keeps the edit, Escape the message as it was),
  // its × removes it; an open edit is drawn again once it closes
  queued.addEventListener("click", function (e) {
    var b = e.target.closest("button");
    if (!b) return;
    if (b.hasAttribute("data-remove")) {
      queueOf(openRef()).remove(Number(b.dataset.remove));
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
    if (area.dataset.drop === undefined) queueOf(openRef()).edit(Number(area.dataset.edit), area.value);
    else queueOf(openRef()).flush();
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
  // Ctrl+k reaches the threads: the first row, the inbox (C14)
  document.addEventListener("keydown", function (e) {
    if (!e.ctrlKey || e.metaKey || e.altKey || e.key !== "k") return;
    var row = rowsEl && rowsEl.querySelector("[data-thread]");
    if (!row) return;
    e.preventDefault();
    if (folded() && window.matchMedia("(max-width: 899px)").matches) fold(false);
    row.focus();
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
    if (!b || busy(openRef())) return;
    var card = b.closest(".proposal");
    if (b.hasAttribute("data-choice")) {
      queueOf(openRef()).add(b.dataset.choice);
    } else if (b.hasAttribute("data-skip")) {
      card.remove();
    } else if (b.hasAttribute("data-doit")) {
      var foot = card.querySelector(".pfoot");
      foot.textContent = "";
      run(open, openOwner, base + "/proposals/" + encodeURIComponent(card.dataset.proposal), "", foot, expired(foot));
    } else if (b.hasAttribute("data-undo")) {
      var out = document.createElement("div");
      out.className = "undoing";
      b.parentNode.after(out);
      b.remove();
      run(open, openOwner, base + "/undo/" + encodeURIComponent(b.dataset.undo), "", out, expired(out));
    }
  });
  // a settled row's or a wake row's fold line opens the row it stands for
  function toggleFold(el) {
    var row = el.closest(".msg");
    var on = row.classList.toggle("expanded");
    el.setAttribute("aria-expanded", on);
  }
  log.addEventListener("click", function (e) {
    var f = e.target.closest("[data-fold]");
    if (f) toggleFold(f);
  });
  log.addEventListener("keydown", function (e) {
    if (e.key !== "Enter" && e.key !== " ") return;
    var f = e.target.closest && e.target.closest("[data-fold]");
    if (f) { e.preventDefault(); toggleFold(f); }
  });

  // load shows thread n, the current one for "": its head and its log,
  // read marks it seen; an archived thread is read-only. keep holds the
  // member's place: a refresh of the thread they stand in moves the log
  // only when they sit at its end; opening lands on the newest rows
  var intro = log.innerHTML;
  function load(n, owner, keep) {
    var url = base + (n === "" ? "" : "?conversation=" + encodeURIComponent(n) +
      (owner ? "&owner=" + encodeURIComponent(owner) : ""));
    return fetch(url, {headers: {Accept: "application/json"}}).then(function (resp) {
      if (!resp.ok) throw new Error("GET " + url + ": " + resp.status);
      return resp.json();
    }).then(function (data) {
      open = data.conversation;
      openThread = data.thread || {number: open};
      openOwner = threadOwner(openThread);
      readOnly = openThread.state === "archived";
      // a stored image's src is its serving route, resolved here where the
      // panel's base lives; the optimistic ones carry their object URL
      data.messages.forEach(function (m) { (m.attachments || []).forEach(function (a) { a.url = a.url || base + "/attachments/" + a.id; }); });
      var was = log.scrollTop, held = log.scrollHeight - log.scrollTop - log.clientHeight < 160;
      log.innerHTML = data.messages.length || data.thread && data.thread.cleared ? chatView.threadHTML(data.messages, login, data.proposals, data.thread) : intro;
      if (form) {
        // an archived thread's composer is one line with restore (C14);
        // another member's archived thread has no restore to offer
        form.classList.toggle("arch", readOnly);
        form.classList.toggle("theirs", !!openOwner);
        form.querySelector(".arch-note").hidden = !readOnly;
      }
      if (busy(openRef())) add(log, chatView.agentHTML(new Date(), "<span class=\"cursor\" aria-hidden=\"true\">▍</span>"));
      pending = readOnly ? null : chatView.pendingOf(data.messages);
      drawAnswer();
      drawHead();
      if (keep && !held) log.scrollTop = was;
      else scrollEnd(true);
      queueOf(openRef()).flush();
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
    head.innerHTML = chatView.headHTML(openThread, login, new Date());
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
      rowsEl.innerHTML = chatView.rowsHTML(list, open, openOwner, login, now);
      sum.innerHTML = chatView.sumHTML(list, login, folded());
      sum.hidden = false;
      var n = chatView.unreadOf(list, login);
      document.title = (n ? "(" + n + ") " : "") + baseTitle;
      var mine = list.filter(function (t) { return t.number === open && threadOwner(t) === openOwner; })[0];
      if (mine && mine.unread && !busy(openRef())) load(open, openOwner);
      else if (mine) {
        openThread = mine;
        drawHead();
      }
    }).catch(function () {});
  }
  // the fold: the table folds to its marks, on the phone to one summary row;
  // folded until the member unfolds it, kept per member in this browser
  function folded() { return panel.classList.contains("sw-collapsed"); }
  function fold(on) {
    panel.classList.toggle("sw-collapsed", on);
    try { localStorage.setItem(foldKey, on ? "1" : "0"); } catch (e) {}
    panel.querySelectorAll("[data-swtoggle]").forEach(function (b) {
      b.setAttribute("aria-expanded", String(!on));
      if (b.classList.contains("swtg")) {
        b.textContent = on ? "›" : "‹";
        b.setAttribute("aria-label", on ? "Unfold the threads" : "Fold the threads");
        b.title = on ? "Unfold the threads" : "Fold the threads";
      }
    });
    sum.innerHTML = chatView.sumHTML(threads, login, on);
  }
  try { fold(localStorage.getItem(foldKey) !== "0"); } catch (e) {}
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
      postThread("/delete", {conversation: n, owner: openOwner}).then(function () { return load(""); }).catch(failed);
    } else if (b || e.target === d) closeDialog();
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && document.getElementById("thread-dialog")) {
      e.stopPropagation();
      closeDialog();
    }
  }, true);
  form.addEventListener("click", function (e) {
    if (!e.target.closest("[data-restore-open]")) return;
    var n = open;
    postThread("/archive", {conversation: n, restore: "1", owner: openOwner}).then(function () { return load(n, openOwner); }).catch(failed);
  });
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
    var whose = row && row.dataset.owner && row.dataset.owner !== login ? row.dataset.owner : "";
    if (row && (Number(row.dataset.thread) !== open || whose !== openOwner)) {
      load(Number(row.dataset.thread), whose).then(function () {
        if (!readOnly) input.focus({preventScroll: true});
      });
    }
  });
  if (answerRow) answerRow.addEventListener("click", function (e) {
    var c = e.target.closest("[data-choice]");
    if (c && pending) {
      var choice = c.dataset.choice;
      pending = null;
      drawAnswer();
      queueOf(openRef()).add(choice);
      return;
    }
    var b = e.target.closest("[data-answer]");
    if (!b || !pending) return;
    var n = open, m = pending;
    pending = null;
    drawAnswer();
    postThread("/answer", {conversation: n, message: m.id, choice: b.dataset.answer, owner: openOwner}).then(function () { return load(n, openOwner); }).catch(failed);
  });
  head.addEventListener("click", function (e) {
    if (e.target.closest("[data-clear]")) {
      postThread("/clear", {conversation: open, owner: openOwner}).then(function () { return load(open, openOwner); }).catch(failed);
      return;
    }
    if (e.target.closest("[data-archive]")) {
      postThread("/archive", {conversation: open, owner: openOwner}).then(function () { return load(""); }).catch(failed);
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
      fetch(base + "/rename", {method: "POST", body: new URLSearchParams({conversation: open, owner: openOwner, title: title}).toString(),
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
  // the list follows the factory: a turn ending elsewhere, a poke arriving,
  // and a settling event at once — the answer row and the badges follow
  // over the page's one stream (the settle concept)
  var settleEvents = {question_answered: 1, session_granted: 1, question_withdrawn: 1, merge_approved: 1, merge_denied: 1,
    plan_accepted: 1, plan_changes_requested: 1, plan_rejected: 1, card_closed: 1, task_ended: 1, approval_requested: 1};
  document.addEventListener("factoryevent", function (e) {
    if (!settleEvents[e.detail.kind]) return;
    listThreads();
    if (open && !busy(openRef()) && !document.hidden && !readOnly) load(open, openOwner, true);
  });
  setInterval(function () {
    if (!document.hidden) listThreads();
  }, 10000);

  load("");
})();
