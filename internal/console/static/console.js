// busy: a form in el is being filled in (a field focused, text typed, a
// choice changed) or a fold or a disclosure in it is open, so a swap of el
// would wipe or close it. The card drawer sits outside the board, so an
// open drawer does not hold the board's swaps: the ages keep ticking.
function busy(el) {
  if (el.querySelector("details[open]:not(.pulse-fold)")) return true;
  if (el.querySelector('button.deny[aria-expanded="true"], button.own-toggle[aria-expanded="true"]')) return true;
  if (el.contains(document.activeElement) && document.activeElement.matches("input, textarea, select")) return true;
  var fields = el.querySelectorAll("input, textarea");
  for (var i = 0; i < fields.length; i++) {
    var f = fields[i];
    if (f.type === "hidden") continue;
    if ((f.type === "radio" || f.type === "checkbox") ? f.checked !== f.defaultChecked : f.value !== f.defaultValue) return true;
  }
  return false;
}
// The board's own requests, the swaps the stream asks for, skip a busy
// board.
document.addEventListener("htmx:beforeRequest", function (e) {
  if (e.detail.elt.id === "board" && busy(e.detail.elt)) e.preventDefault();
});
// A board that loaded more finished cards keeps them: what answers the
// board, a swap or an action, reads as many of each column's as it shows.
document.addEventListener("htmx:configRequest", function (e) {
  if (!e.detail.target || e.detail.target.id !== "board") return;
  document.querySelectorAll("#board [data-finished]").forEach(function (col) {
    e.detail.parameters[col.dataset.finished + "-shown"] = col.querySelectorAll("[data-card]").length;
  });
});

// A refused board action (a move the lane does not take from you) answers
// one sentence; htmx swaps nothing on an error, so show it.
document.addEventListener("htmx:responseError", function (e) {
  var flash = document.getElementById("flash");
  if (!flash) return;
  flash.textContent = e.detail.xhr.responseText.trim() || "The factory refused: " + e.detail.xhr.status;
  flash.hidden = false;
});
// A refusal that answers the board (a plan revision gone stale) is
// swapped in: its line stands on the card it concerns.
document.addEventListener("htmx:beforeSwap", function (e) {
  if (e.detail.xhr.status === 409 && e.detail.xhr.responseText.indexOf("<div id=\"board\"") === 0) {
    e.detail.shouldSwap = true;
    e.detail.isError = false;
  }
});
document.addEventListener("htmx:beforeRequest", function (e) {
  var flash = document.getElementById("flash");
  if (flash && e.detail.elt.id !== "board") flash.hidden = true;
});

// Drag a backlog card onto a lane column: submits the card's own move form
// for that lane, the same request as its button. Cards without a form for
// the lane are not dropped there.
document.addEventListener("dragstart", function (e) {
  var card = e.target.closest && e.target.closest("[data-card][draggable]");
  if (card) e.dataTransfer.setData("text/plain", card.dataset.card);
});
function moveForm(e) {
  var column = e.target.closest && e.target.closest("[data-lane]");
  if (!column) return null;
  var card = document.querySelector("[data-card=\"" + CSS.escape(e.dataTransfer.getData("text/plain")) + "\"]");
  return card && card.querySelector("form[data-move=\"" + CSS.escape(column.dataset.lane) + "\"]");
}
document.addEventListener("dragover", function (e) {
  if (e.target.closest && e.target.closest("[data-lane]")) e.preventDefault();
});
document.addEventListener("drop", function (e) {
  var form = moveForm(e);
  if (!form) return;
  e.preventDefault();
  form.requestSubmit();
});

// The new-task form offers the lanes of the chosen project's process only.
function offerLanes() {
  var project = document.getElementById("project"), lane = document.getElementById("lane");
  if (!project || !lane) return;
  for (var i = 0; i < lane.options.length; i++) {
    var o = lane.options[i];
    o.disabled = o.dataset.projects !== undefined && o.dataset.projects.split(" ").indexOf(project.value) < 0;
  }
  if (lane.selectedOptions[0] && lane.selectedOptions[0].disabled) lane.value = "ready";
}
document.addEventListener("change", function (e) {
  if (e.target.id === "project") offerLanes();
  // a setting applies at once, with no button to press: the board's
  // project, a column's order and show, a card's accent
  var f = e.target.form;
  if (f && f.classList.contains("auto-submit")) f.requestSubmit();
});
document.addEventListener("DOMContentLoaded", offerLanes);

// Theme selector (C15): the moon sets dark, the default, the sun sets
// light; the choice is remembered in localStorage, which theme.js applies
// before the first paint. aria-pressed names the theme in force.
(function () {
  var group = document.getElementById("theme-switch");
  if (!group) return;
  var buttons = group.querySelectorAll("button[data-theme-set]");
  function mark(theme) {
    buttons.forEach(function (b) { b.setAttribute("aria-pressed", String(b.dataset.themeSet === theme)); });
  }
  mark(document.documentElement.getAttribute("data-theme"));
  group.addEventListener("click", function (e) {
    var b = e.target.closest("button[data-theme-set]");
    if (!b) return;
    var theme = b.dataset.themeSet;
    document.documentElement.setAttribute("data-theme", theme);
    localStorage.setItem("theme", theme);
    mark(theme);
  });
})();

// The board follows its project's event stream, and no timer runs: an
// event swaps the cards it names from GET /cards/{id} at once, or the
// whole board when the card is not on it or its place changed, and
// card_filed, project_merged, event board (a face changed with time, or
// events came to a member who only reads) and a reconnect swap the board,
// whose answer carries the status line too. counters sets the status line
// and the columns' counts the server read; the agent's text and the
// steers reach the drawer's Chat. A swap a busy card or board would wipe
// waits until the focus lands outside a field, a fold or the drawer
// closes; the footer says when the stream is down.
(function () {
  var missed = {}, latest = {}, asked = 0;
  function board() { return document.getElementById("board"); }
  function retry() {
    var ids = Object.keys(missed);
    missed = {};
    ids.forEach(function (id) {
      if (id) swapCard(id);
      else swapBoard();
    });
  }
  // focus that moves on to another field holds the swap: it waits for focusin
  document.addEventListener("focusin", retry);
  document.addEventListener("focusout", function (e) {
    if (!e.relatedTarget) retry();
  });
  document.addEventListener("toggle", retry, true);
  document.addEventListener("drawerclosed", retry);
  function swapBoard() {
    var b = board();
    if (busy(b)) {
      missed[""] = true;
      return;
    }
    htmx.ajax("GET", "/board" + b.dataset.query, {source: "#board", target: "#board", swap: "outerHTML"});
  }
  function swapCard(id) {
    var cards = document.querySelectorAll("[data-card=\"" + CSS.escape(id) + "\"]");
    if (!cards.length) return swapBoard();
    for (var i = 0; i < cards.length; i++) {
      if (busy(cards[i])) {
        missed[id] = true;
        return;
      }
    }
    var n = latest[id] = ++asked;
    fetch("/cards/" + encodeURIComponent(id) + board().dataset.query).then(function (r) {
      if (!r.ok) throw new Error("GET /cards/" + id + ": " + r.status);
      return r.text();
    }).then(function (html) {
      if (latest[id] !== n) return;
      var fresh = document.createElement("template");
      fresh.innerHTML = html;
      if (!fresh.content.firstElementChild || fresh.content.firstElementChild.dataset.place !== cards[0].dataset.place) return swapBoard();
      cards.forEach(function (k) {
        if (k.isConnected) htmx.swap(k, html, {swapStyle: "outerHTML"});
      });
    }).catch(swapBoard);
  }
  // counters are the status line and the columns' counts as the server
  // read them, set whatever the board's state: neither holds a form
  function counters(d) {
    var line = document.getElementById("status-line"), counts = document.querySelectorAll("#board .count");
    // a swap under the keyboard's focus would drop it: that panel waits for the next
    if (line && !line.contains(document.activeElement)) {
      line.innerHTML = d.status;
      document.dispatchEvent(new Event("statusswapped"));
    }
    if (counts.length === d.counts.length) d.counts.forEach(function (n, i) { counts[i].innerHTML = n; });
  }
  // chat loads the drawer's Chat again while it shows card id's session,
  // any card's for "", the steer being typed kept
  function chat(id) {
    var log = document.getElementById("session-log"), box = document.getElementById("steer-input"), typed = box && box.value;
    if (!log || id && log.dataset.task !== id) return;
    htmx.ajax("GET", "/cards/" + encodeURIComponent(log.dataset.task) + "/drawer?tab=chat", {target: "#drawer", swap: "innerHTML"}).then(function () {
      var again = document.getElementById("steer-input");
      if (typed && again) again.value = typed;
    });
  }
  // said adds the agent's text or a steer to the drawer's Chat while it
  // shows that card's session, once; another session's text loads the
  // tab again
  function said(e) {
    var log = document.getElementById("session-log"), d = e.detail, agent = e.kind === "session_said";
    if (!log || log.dataset.task !== e.task_id) return;
    if (agent && d.session !== log.dataset.session) return chat(e.task_id);
    var key = agent ? d.session + "@" + e.at : "e" + e.id, box = document.getElementById("drawer");
    if (log.querySelector("[data-key=\"" + CSS.escape(key) + "\"]")) return;
    var end = box.scrollHeight - box.scrollTop - box.clientHeight < 160;
    var msg = log.appendChild(document.createElement("div"));
    msg.className = "msg " + (agent ? "agent" : "you");
    msg.dataset.key = key;
    var av = msg.appendChild(document.createElement("span")), who = msg.appendChild(document.createElement("p")), text = msg.appendChild(document.createElement("p"));
    av.className = "av";
    av.setAttribute("aria-hidden", "true");
    av.textContent = agent ? "" : String(e.actor || "").charAt(0).toUpperCase();
    who.className = "who";
    var name = who.appendChild(document.createElement("span")), when = who.appendChild(document.createElement("time"));
    name.className = "name";
    name.textContent = agent ? "agent" : e.actor;
    when.textContent = new Date(e.at).toISOString().slice(5, 16).replace("T", " ") + "Z";
    text.className = "said";
    text.textContent = d.text || "";
    if (end) box.scrollTop = box.scrollHeight;
  }
  function follow(project, query) {
    var es = new EventSource("/projects/" + encodeURIComponent(project) + "/events/stream" + query), opened = false;
    es.onopen = function () {
      document.getElementById("live").textContent = "";
      // the replay after a reconnect holds at most the newest 500 events;
      // the agent's text is never replayed, so the Chat is read again
      if (opened) {
        swapBoard();
        chat("");
      }
      opened = true;
    };
    es.onerror = function () {
      document.getElementById("live").textContent = es.readyState === EventSource.CLOSED ?
        "The live stream ended: reload the page to follow the board." : "The live stream is down; it reconnects by itself.";
    };
    es.addEventListener("event", function (m) {
      var e = JSON.parse(m.data);
      if (e.kind === "pulse_computing" || e.kind === "pulse") pulse(e);
      else if (e.kind === "session_said") said(e);
      else if (!e.task_id || e.kind === "card_filed" || e.kind === "project_merged") swapBoard();
      else {
        if (e.kind === "card_steered") said(e);
        // a new status may end the session, and its steer box with it
        if (e.kind === "task_status") chat(e.task_id);
        swapCard(e.task_id);
      }
    });
    es.addEventListener("counters", function (m) { counters(JSON.parse(m.data)); });
    es.addEventListener("board", function () { swapBoard(); });
  }
  // The pulse band: pulse_computing shows its typing indicator, pulse
  // swaps its sentence and bullets in; the band's disclosure (the status
  // in detail with the bullets) stays as the member left it across the
  // counters' swaps of the strip.
  var statusOpen = false, pulseBusy = false;
  function showStatus() {
    var panel = document.getElementById("status-panel"), bullets = document.getElementById("pulse-bullets"),
      toggle = document.getElementById("status-toggle"), typing = document.querySelector && document.querySelector("#pulse .typing");
    if (panel) panel.hidden = !statusOpen;
    if (bullets) bullets.hidden = !statusOpen;
    if (toggle) {
      toggle.setAttribute("aria-expanded", String(statusOpen));
      toggle.parentElement.classList.toggle("xp-open", statusOpen);
    }
    if (typing) typing.hidden = !pulseBusy;
  }
  function toggleStatus(open) {
    statusOpen = open === undefined ? !statusOpen : open;
    showStatus();
  }
  document.addEventListener("htmx:afterSettle", showStatus);
  document.addEventListener("statusswapped", showStatus);
  document.addEventListener("click", function (e) {
    var band = e.target.closest && e.target.closest("#pulse");
    if (!band) return;
    if (e.target.closest("#status-toggle")) return toggleStatus();
    // a click anywhere on the band that is not a link, a button or a field
    if (e.target.closest("a, button, input, select, textarea, label, form")) return;
    toggleStatus();
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && statusOpen && !e.target.closest("input, textarea, select")) {
      var drawer = document.getElementById("drawer"), keys = document.getElementById("keys");
      if (drawer && !drawer.hidden || keys && !keys.hidden) return;
      toggleStatus(false);
      var t = document.getElementById("status-toggle");
      if (t) t.focus();
    }
  });
  function pulse(e) {
    var el = document.getElementById("pulse"), d = e.detail;
    pulseBusy = e.kind === "pulse_computing";
    if (!pulseBusy && d.sentence && !el) return swapBoard();
    if (!pulseBusy && d.sentence) {
      el.classList.toggle("prominent", !!d.prominent);
      var sign = el.querySelector(".pulse-sign"), say = el.querySelector(".pulse-say"), ul = document.getElementById("pulse-bullets");
      if (sign) sign.hidden = !d.prominent;
      if (say) say.textContent = d.sentence;
      if (ul) {
        ul.textContent = "";
        (d.bullets || []).forEach(function (b) { ul.appendChild(document.createElement("li")).textContent = b; });
      }
    }
    showStatus();
  }
  document.addEventListener("DOMContentLoaded", function () {
    var b = board();
    if (b && b.dataset.project) follow(b.dataset.project, b.dataset.query);
  });
})();

// The whole card opens the drawer (item 226): a click anywhere on the
// face that is not a link, a button or a field; the title's link is the
// same door for the keyboard. A selection is left alone.
document.addEventListener("click", function (e) {
  if (e.target.closest("a, button, input, select, textarea, label, summary, form, details")) return;
  var card = e.target.closest("#board [data-card]");
  // a card without the title's link belongs to a reader, who has no drawer
  if (!card || !card.querySelector("a.open") || String(getSelection && getSelection())) return;
  location.hash = "#card=" + card.dataset.card;
});
// On the phone the columns are sections: Done starts folded, a tap on a
// column's title folds or unfolds it.
(function () {
  var folded = {};
  function narrow() { return window.innerWidth < 900; }
  function fold() {
    document.querySelectorAll("#board .column.fold").forEach(function (c) {
      var id = c.getAttribute("aria-labelledby") || "", h = c.querySelector(":scope > h3");
      if (!(id in folded)) folded[id] = narrow();
      c.classList.toggle("folded", folded[id]);
      if (!h) return;
      h.setAttribute("aria-expanded", String(!folded[id]));
      if (narrow()) { h.setAttribute("role", "button"); h.tabIndex = 0; } else { h.removeAttribute("role"); h.removeAttribute("tabindex"); }
    });
  }
  function toggle(h) {
    var c = h.parentElement, id = c.getAttribute("aria-labelledby") || "";
    folded[id] = !folded[id];
    fold();
  }
  document.addEventListener("DOMContentLoaded", fold);
  document.addEventListener("htmx:afterSettle", fold);
  window.addEventListener("resize", fold);
  document.addEventListener("click", function (e) {
    var h = e.target.closest && e.target.closest("#board .column.fold > h3");
    if (h && narrow()) toggle(h);
  });
  document.addEventListener("keydown", function (e) {
    var h = e.target.closest && e.target.closest("#board .column.fold > h3");
    if (h && narrow() && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); toggle(h); }
  });
})();

// A disclosure: a button with aria-controls reveals or hides the element it
// names (the deny form, the own-answer box) and says so in aria-expanded.
document.addEventListener("click", function (e) {
  var b = e.target.closest("button.deny[aria-controls], button.own-toggle[aria-controls]");
  if (!b || b.type === "submit") return;
  var box = document.getElementById(b.getAttribute("aria-controls"));
  if (!box) return;
  var open = b.getAttribute("aria-expanded") !== "true";
  b.setAttribute("aria-expanded", String(open));
  box.hidden = !open;
  if (open) { var f = box.querySelector("textarea, input, select"); if (f) f.focus(); }
});

// The card drawer docked at the right edge over the board: #card=<id> in
// the URL opens it, on load too, and a card links there; #card=<id>:<section>
// opens it with that section of the overview open. Escape or its ✕
// closes it.
// Its tabs and related cards load into it by hx-get, and the URL follows
// the card it shows.
function drawer() { return document.getElementById("drawer"); }
function showDrawer() {
  var d = drawer(), m = /^#card=([^:]+)(?::([a-z]+))?$/.exec(location.hash);
  if (!d) return;
  document.body.classList.toggle("drawer-open", !!m);
  d.hidden = !m;
  if (!m) {
    d.textContent = "";
    document.dispatchEvent(new Event("drawerclosed"));
    return;
  }
  var id;
  try { id = decodeURIComponent(m[1]); } catch (err) { closeDrawer(); return; }
  htmx.ajax("GET", "/cards/" + encodeURIComponent(id) + "/drawer" + (m[2] ? "?tab=overview" : ""), {target: "#drawer", swap: "innerHTML"}).then(function () {
    var sec = m[2] && d.querySelector('details[data-sec="' + m[2] + '"]');
    if (sec) { sec.open = true; sec.querySelector("summary").focus(); return; }
    var h = d.querySelector("h2");
    if (h) h.focus();
  });
}
function closeDrawer() {
  if (location.hash.indexOf("#card=") !== 0) return;
  history.replaceState(null, "", location.pathname + location.search);
  showDrawer();
}
window.addEventListener("hashchange", showDrawer);
document.addEventListener("DOMContentLoaded", showDrawer);
// a drawer refused or unreachable closes, so the board moves again; a
// refused steer leaves it open, its sentence in the flash
function drawerFailed(e) {
  if (e.detail.target && e.detail.target.id === "drawer" && e.detail.requestConfig.verb === "get") closeDrawer();
}
document.addEventListener("htmx:responseError", drawerFailed);
document.addEventListener("htmx:sendError", drawerFailed);
// after a tab the focus is on the tab shown, after a steer in its input
document.addEventListener("htmx:afterSwap", function (e) {
  var t = e.detail.target, shown = t && t.id === "drawer" && t.querySelector("[data-drawer]");
  if (!shown) return;
  if (location.hash !== "#card=" + shown.dataset.drawer) history.replaceState(null, "", "#card=" + shown.dataset.drawer);
  var from = e.detail.requestConfig.elt, steer = document.getElementById("steer-input");
  if (from.getAttribute("role") === "tab") t.querySelector("[role=tab][aria-selected=true]").focus();
  else if (from.id === "steer-form" && steer) steer.focus();
});

// The board's keys, listed by ? : w switches the view, Escape closes the
// list or the drawer. The view chosen is remembered (theme.js applies it).
(function () {
  function keys() { return document.getElementById("keys"); }
  function showKeys(open) {
    var k = keys();
    if (!k) return;
    k.hidden = !open;
    if (open) document.getElementById("keys-close").focus();
  }
  document.addEventListener("keydown", function (e) {
    if (!document.getElementById("board") || e.ctrlKey || e.metaKey || e.altKey) return;
    // Escape in the find box clears it when it holds a find, else leaves it
    if (e.key === "Escape" && e.target.id === "q") {
      if (e.target.value) { e.target.value = ""; e.target.form.submit(); } else e.target.blur();
      return;
    }
    if (e.key === "Escape") {
      if (keys() && !keys().hidden) showKeys(false);
      else closeDrawer();
      return;
    }
    if (e.target.closest("input, textarea, select, [contenteditable]")) return;
    if (e.key === "w" && window.innerWidth < 900) return;
    if (e.key === "w") {
      e.preventDefault();
      document.getElementById("view-toggle").click();
    } else if (e.key === "?") {
      e.preventDefault();
      showKeys(true);
    } else if (e.key === "f") {
      var q = document.getElementById("q");
      if (!q) return;
      e.preventDefault();
      q.focus();
      q.select();
    }
  });
  document.addEventListener("click", function (e) {
    if (e.target.closest("#keys-toggle")) showKeys(true);
    else if (e.target.closest("#keys-close") || e.target.id === "keys") showKeys(false);
    else if (e.target.closest("#drawer-close")) closeDrawer();
    var toggle = e.target.closest("#view-toggle");
    if (!toggle) return;
    if (toggle.dataset.view) localStorage.setItem("view", toggle.dataset.view);
    else localStorage.removeItem("view");
  });
})();

// The column menus (C13): a choice submits the menu's form (auto-submit),
// so the board reloads with the column's order and show in its query;
// Escape or a click outside closes an open menu. An open menu holds the
// board's swaps, as any open disclosure does.
document.addEventListener("click", function (e) {
  document.querySelectorAll("details.col-menu[open]").forEach(function (d) { if (!d.contains(e.target)) d.open = false; });
});
document.addEventListener("keydown", function (e) {
  if (e.key !== "Escape") return;
  var d = e.target.closest && e.target.closest("details.col-menu[open]");
  if (!d) return;
  d.open = false;
  d.querySelector("summary").focus();
  e.stopPropagation();
}, true);

// The project's documents (roadmap 277, the 2026-10-15 plan): the
// Documents button and a document's own links open one in the view docked
// over the board — never over the chat — rendered from
// GET /projects/{name}/docs/{path}. ?doc=<path> in the URL holds it, so a
// reload and the back button stand where you were, and a card's #card=
// hash takes the slot: the document hides while the card's drawer shows
// and comes back when it closes. Under the head the path line carries the
// document's path, its short commit and its time of day; the pins row
// holds a tab per pinned document — the backlog file pinned by default —
// in localStorage per project. Recent documents and each one's scroll
// are remembered per project; a hover of a link peeks its first
// paragraph after 400ms, never on a coarse pointer or a narrow screen.
(function () {
  var nav = { stack: [], index: -1 }, peeks = {}, hoverTimer = null, peekEl = null, peekFor = null;
  // the editor (the 2026-10-15 plan, item 3): versions hold each
  // document's last read or landed save — its text, content hash, commit
  // and time — saves each path's save state (busy, queued, the refusal,
  // the landed commit), and edit is the open document's mode. The draft
  // itself lives in localStorage, so it outlives the tab; the dot — on
  // the path line, the pins, Recent and the Documents button — says a
  // draft is there from the first keystroke to the save that lands.
  var versions = {}, saves = {}, edit = { path: "", mode: "read" }, confirmAsk = null, restoredFor = "";

  function view() { return document.getElementById("doc-view"); }
  function board() { return document.getElementById("board"); }
  function project() { return board() && board().dataset.project || ""; }
  function visible() { var v = view(); return !!v && !v.hidden; }
  function label(path) { return path.split("/").pop().replace(/\.md$/, ""); }
  function current() { return nav.stack[nav.index] || ""; }
  function timeOfDay(at) { var d = at ? new Date(at) : new Date(); return ("0" + d.getHours()).slice(-2) + ":" + ("0" + d.getMinutes()).slice(-2); }

  // the draft: localStorage per project and path, the text and the time
  // it began
  function draftKey(path) { return "doc:" + project() + ":draft:" + path; }
  function draftOf(path) {
    try {
      var v = localStorage.getItem(draftKey(path));
      var d = v ? JSON.parse(v) : null;
      return d && typeof d.text === "string" ? d : null;
    } catch (err) { return null; }
  }
  function keepDraft(path, text) {
    try {
      var d = draftOf(path);
      localStorage.setItem(draftKey(path), JSON.stringify({ text: text, at: d ? d.at : timeOfDay() }));
    } catch (err) {}
  }
  function dropDraft(path) { try { localStorage.removeItem(draftKey(path)); } catch (err) {} }
  function anyDraft() {
    try {
      var pre = "doc:" + project() + ":draft:";
      for (var i = 0; i < localStorage.length; i++) {
        if (localStorage.key(i).indexOf(pre) === 0) return true;
      }
    } catch (err) {}
    return false;
  }

  function load(key, fallback) {
    try {
      var v = sessionStorage.getItem("doc:" + project() + ":" + key);
      return v ? JSON.parse(v) : fallback;
    } catch (err) { return fallback; }
  }
  function store(key, value) {
    try { sessionStorage.setItem("doc:" + project() + ":" + key, JSON.stringify(value)); } catch (err) {}
  }
  // the pins outlive the tab: localStorage, with the backlog file pinned
  // until the member pins another
  function pins() {
    try {
      var v = localStorage.getItem("doc:" + project() + ":pins");
      var p = v ? JSON.parse(v) : null;
      if (Array.isArray(p) && p.length) return p;
    } catch (err) {}
    return ["docs/roadmap.md"];
  }
  function storePins(list) {
    try { localStorage.setItem("doc:" + project() + ":pins", JSON.stringify(list)); } catch (err) {}
  }
  function syncPins() {
    var navEl = document.getElementById("doc-pins");
    if (!navEl) return;
    var current = nav.stack[nav.index] || "";
    navEl.textContent = "";
    var l = navEl.appendChild(document.createElement("span"));
    l.className = "doc-pins-l";
    l.setAttribute("aria-hidden", "true");
    l.textContent = "pins";
    var list = pins();
    list.forEach(function (p) {
      var b = navEl.appendChild(document.createElement("button"));
      b.type = "button";
      b.className = "doc-pin";
      b.dataset.doc = p;
      b.title = p;
      if (p === current) b.setAttribute("aria-current", "page");
      if (draftOf(p)) {
        b.appendChild(dotSpan());
        b.appendChild(document.createTextNode(" "));
      }
      b.appendChild(document.createTextNode(label(p)));
    });
    if (current && list.indexOf(current) < 0) {
      var add = navEl.appendChild(document.createElement("button"));
      add.type = "button";
      add.className = "doc-pin-add";
      add.title = "Pin " + current;
      add.textContent = "+ pin";
    }
  }
  function pinOpen() {
    var path = nav.stack[nav.index];
    if (!path) return;
    var list = pins();
    if (list.indexOf(path) < 0) {
      list.push(path);
      storePins(list);
    }
    syncPins();
  }
  function docURL(path) {
    return "/projects/" + encodeURIComponent(project()) + "/docs/" + path.split("/").map(encodeURIComponent).join("/");
  }
  function urlPath(path) {
    var u = new URL(location.href);
    if (path) u.searchParams.set("doc", path); else u.searchParams.delete("doc");
    return u.pathname + u.search;
  }
  function docParam() {
    var m = /[?&]doc=([^&#]*)/.exec(location.search || "");
    if (!m) return "";
    try { return decodeURIComponent(m[1]); } catch (err) { return m[1]; }
  }
  // the draft's dot: the accent colour everywhere it shows — the path
  // line, a pin, a Recent row, the Documents button
  function dotSpan() {
    var d = document.createElement("span");
    d.className = "doc-dot";
    d.setAttribute("role", "img");
    d.setAttribute("aria-label", "unsaved");
    d.textContent = "●";
    return d;
  }
  function syncButton(open) {
    var b = document.getElementById("documents");
    if (!b) return;
    b.setAttribute("aria-expanded", String(open));
    // the dot: a draft waits in the editor
    b.textContent = "";
    if (anyDraft()) {
      b.appendChild(dotSpan());
      b.appendChild(document.createTextNode(" "));
    }
    b.appendChild(document.createTextNode("Documents"));
  }
  function syncNav() {
    var back = document.getElementById("doc-back"), fwd = document.getElementById("doc-fwd");
    if (!back || !fwd) return;
    var prev = nav.index > 0 ? nav.stack[nav.index - 1] : "", next = nav.index < nav.stack.length - 1 ? nav.stack[nav.index + 1] : "";
    back.disabled = !prev;
    fwd.disabled = !next;
    back.setAttribute("aria-label", prev ? "Back to " + label(prev) : "Nothing back");
    back.title = prev ? "Back: " + label(prev) : "Nothing back";
    fwd.setAttribute("aria-label", next ? "Forward to " + label(next) : "Nothing forward");
    fwd.title = next ? "Forward: " + label(next) : "Nothing forward";
  }
  function syncRecent(current) {
    var menu = document.getElementById("doc-recent-menu");
    if (!menu) return;
    menu.textContent = "";
    var list = load("recent", []);
    if (!list.length) {
      menu.appendChild(document.createElement("span")).className = "doc-recent-none";
      menu.firstChild.textContent = "Nothing yet";
      return;
    }
    list.forEach(function (p) {
      var row = menu.appendChild(document.createElement("button"));
      row.type = "button";
      row.className = "doc-recent-row";
      row.dataset.doc = p;
      row.setAttribute("role", "menuitem");
      row.appendChild(document.createElement("span")).className = "doc-recent-label";
      // ✓ the open document, ● a draft of it waiting in the editor
      if (p === current) row.firstChild.appendChild(document.createTextNode("✓ "));
      if (draftOf(p)) {
        row.firstChild.appendChild(dotSpan());
        row.firstChild.appendChild(document.createTextNode(" "));
      }
      row.firstChild.appendChild(document.createTextNode(label(p)));
    });
  }
  function remember(path) {
    var list = load("recent", []).filter(function (p) { return p !== path; });
    list.unshift(path);
    store("recent", list.slice(0, 6));
    syncRecent(path);
  }
  function keepScroll() {
    var v = view(), path = nav.stack[nav.index];
    if (v && path) store("scroll:" + path, v.querySelector("#doc-article").scrollTop);
  }

  function show(d, frag) {
    var v = view(), art = document.getElementById("doc-article");
    if (!v || !art) return;
    var path = d.path, draft = draftOf(path);
    versions[path] = { text: null, html: d.html, sha256: d.sha256, commit: d.commit, at: d.at, forgejo: d.forgejo, title: d.title };
    art.textContent = "";
    art.innerHTML = d.html;
    var pathEl = document.getElementById("doc-path");
    pathEl.textContent = d.path;
    pathEl.title = d.path;
    var link = document.getElementById("doc-forgejo");
    if (d.forgejo) { link.hidden = false; link.href = d.forgejo; } else link.hidden = true;
    v.classList.toggle("wide", !!load("wide", false));
    document.getElementById("doc-wide").setAttribute("aria-pressed", String(v.classList.contains("wide")));
    syncNav();
    art.scrollTop = 0;
    var at = frag && art.querySelector('[id="' + CSS.escape(frag) + '"]');
    if (at) at.scrollIntoView();
    else art.scrollTop = load("scroll:" + d.path, 0) || 0;
    // a draft reopens in the editor, the note saying it was restored
    if (draft) { restoredFor = path; enterEdit(); } else setMode("read");
    v.focus({ preventScroll: true });
  }
  function failDoc(text, status) {
    var art = document.getElementById("doc-article");
    if (!art) return;
    art.textContent = "";
    art.appendChild(document.createElement("p")).className = "doc-none";
    art.firstChild.textContent = text.trim() || "The document cannot be read (" + status + ").";
  }

  // the editor and its states (the 2026-10-15 plan, item 3): the mode
  // buttons, the textarea with its row — Save as commit, Cancel, Discard,
  // based on — and the note line under the head. The path line carries
  // the state: the dot from the first keystroke, saving…, the amber
  // reason a save refused, the landed commit, else the version's commit
  // and time. One save is in flight; one more is queued behind it and
  // only runs when the member kept typing.
  function setMode(mode) {
    edit.mode = mode;
    var art = document.getElementById("doc-article"), box = document.getElementById("doc-edit");
    var read = document.getElementById("doc-mode-read"), ed = document.getElementById("doc-mode-edit");
    if (!art || !box || !read || !ed) return;
    var ver = versions[current()];
    art.hidden = mode !== "read";
    box.hidden = mode !== "edit";
    read.setAttribute("aria-pressed", String(mode === "read"));
    ed.setAttribute("aria-pressed", String(mode === "edit"));
    read.disabled = !ver;
    read.title = ver ? "" : "Not saved yet";
    syncDoc();
  }
  function enterEdit() {
    var path = current(), ver = versions[path], draft = draftOf(path);
    var ed = document.getElementById("doc-editor");
    if (!ed) return;
    // the text the editor starts from: the draft, else the version's,
    // fetched raw — a new file starts from nothing
    if (draft) { ed.value = draft.text; setMode("edit"); ed.focus({ preventScroll: true }); return; }
    if (ver && ver.text !== null) { ed.value = ver.text; setMode("edit"); ed.focus({ preventScroll: true }); return; }
    ed.value = ver ? "Reading " + label(path) + "…" : "";
    setMode("edit");
    if (!ver) { ed.value = ""; ed.focus({ preventScroll: true }); return; }
    fetch(docURL(path) + "?raw=1").then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); }).then(function (text) {
      if (current() !== path || versions[path] !== ver) return;
      ver.text = text;
      if (!draftOf(path)) { ed.value = text; syncDoc(); }
      ed.focus({ preventScroll: true });
    }).catch(function () {
      if (current() === path) { ed.value = ""; syncDoc(); }
    });
  }
  // the path line's state and the dot before the path
  function syncDoc() {
    var path = current(), ver = versions[path], s = saves[path] || {};
    var draft = !!draftOf(path);
    var dot = document.getElementById("doc-dot");
    if (dot) dot.hidden = !draft;
    var facts = document.getElementById("doc-facts");
    if (facts) {
      facts.textContent = "";
      facts.className = "doc-facts";
      facts.title = "";
      if (s.busy) facts.textContent = "· saving…";
      else if (s.err) {
        facts.className = "doc-facts doc-refused";
        facts.textContent = "not saved: " + s.err.why + " · retry";
        facts.title = "the save did not land; Retry sends it again";
      } else if (s.landed) {
        facts.className = "doc-facts doc-saved";
        facts.textContent = "Saved: " + s.landed.sha.slice(0, 7) + " on master · " + s.landed.at;
        facts.title = "commit " + s.landed.sha;
      } else if (ver && !draft && ver.commit) {
        facts.textContent = ver.commit.slice(0, 7) + " · " + timeOfDay(ver.at);
        facts.title = "content " + ver.sha256 + "\ncommit " + ver.commit;
      }
    }
    syncPins();
    syncRecent(path);
    syncNote();
    syncEditRow();
    syncButton(visible());
  }
  // the note line: a confirm ask, else the restored draft
  function syncNote() {
    var note = document.getElementById("doc-note");
    if (!note) return;
    note.textContent = "";
    note.hidden = true;
    if (!current()) return;
    if (confirmAsk && confirmAsk.at === "note") { note.hidden = false; note.appendChild(confirmHTML()); return; }
    var draft = draftOf(current());
    if (restoredFor === current() && draft) {
      note.hidden = false;
      note.appendChild(document.createTextNode("draft from " + draft.at + " restored · "));
      note.appendChild(noteLink("discard", "discard"));
    }
  }
  function noteLink(act, text) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "linkbtn";
    b.dataset.act = act;
    b.textContent = text;
    return b;
  }
  function confirmHTML() {
    var span = document.createElement("span");
    span.className = "doc-confirm";
    span.setAttribute("role", "group");
    span.setAttribute("aria-label", "Discard your changes?");
    span.appendChild(document.createElement("span")).className = "doc-confirm-q";
    span.firstChild.textContent = "Discard your changes?";
    var yes = span.appendChild(document.createElement("button"));
    yes.type = "button";
    yes.className = "deny";
    yes.dataset.act = "discard-yes";
    yes.textContent = "Discard";
    var no = span.appendChild(document.createElement("button"));
    no.type = "button";
    no.dataset.act = "confirm-no";
    no.textContent = "Keep";
    return span;
  }
  // the editor's row: the ask while one is open, else Save as commit,
  // Cancel, Discard when a draft is there, and what it is based on
  function syncEditRow() {
    var row = document.getElementById("doc-edit-row");
    if (!row || edit.mode !== "edit" || !current()) return;
    row.textContent = "";
    if (confirmAsk && confirmAsk.at === "row") { row.appendChild(confirmHTML()); return; }
    var save = row.appendChild(document.createElement("button"));
    save.type = "button";
    save.className = "primary";
    save.dataset.act = "save";
    save.title = "Ctrl+S";
    save.setAttribute("aria-keyshortcuts", "Control+S Meta+S");
    save.textContent = "Save as commit";
    var cancel = row.appendChild(document.createElement("button"));
    cancel.type = "button";
    cancel.dataset.act = "cancel";
    cancel.textContent = "Cancel";
    if (draftOf(current())) {
      var drop = row.appendChild(document.createElement("button"));
      drop.type = "button";
      drop.className = "linkbtn doc-discard";
      drop.dataset.act = "discard";
      drop.textContent = "Discard";
    }
    var base = row.appendChild(document.createElement("span"));
    base.className = "doc-base";
    var ver = versions[current()];
    base.textContent = ver && ver.commit ? "based on " + ver.commit.slice(0, 7) : "a new file: the first save creates it";
  }
  // the text a save would take: the editor's, else the draft's
  function typedText(path) {
    var ed = document.getElementById("doc-editor");
    if (ed && edit.mode === "edit" && current() === path) return ed.value;
    var d = draftOf(path);
    return d ? d.text : versions[path] && versions[path].text || "";
  }
  function saveDoc(path) {
    if (!path) return;
    var s = saves[path] = saves[path] || {};
    if (s.busy) { s.queued = true; return; }
    var ver = versions[path];
    var text = typedText(path);
    s.busy = true; s.queued = false; s.err = null; s.landed = null;
    confirmAsk = null;
    syncDoc();
    var csrf = document.querySelector('input[name="csrf"]');
    fetch(docURL(path), {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json", "X-CSRF-Token": csrf ? csrf.value : "" },
      body: JSON.stringify({ text: text, base: ver ? ver.sha256 || "" : "", commit: ver ? ver.commit || "" : "" })
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) { if (!r.ok) throw j; return j; });
    }).then(function (ans) { landed(path, text, ans); }).catch(function (j) { failed(path, j); });
  }
  // a save landed: the answer's hash, commit and time become the
  // version; the draft goes when nothing more was typed, and one queued
  // save runs when something was. With nothing typed since, the view
  // returns to Read and renders the saved version, the path line keeping
  // its Saved line
  function landed(path, text, ans) {
    var s = saves[path] = saves[path] || {};
    s.busy = false;
    versions[path] = { text: text, html: null, sha256: ans.sha256, commit: ans.commit, at: ans.at };
    s.landed = { sha: ans.commit, at: timeOfDay(ans.at) };
    if (restoredFor === path) restoredFor = "";
    var typed = typedText(path);
    if (typed === text) dropDraft(path); else keepDraft(path, typed);
    if (s.queued) {
      s.queued = false;
      if (typed !== text) { saveDoc(path); return; }
    }
    if (typed === text && edit.mode === "edit" && current() === path && visible()) { readMode(); return; }
    syncDoc();
  }
  function failed(path, j) {
    var s = saves[path] = saves[path] || {};
    s.busy = false;
    s.queued = false;
    s.err = { why: j && j.reason || "the save did not land; try again" };
    syncDoc();
  }
  function openDoc(path, opts) {
    opts = opts || {};
    var v = view();
    if (!v || !project() || !path) return;
    if (location.hash.indexOf("#card=") === 0) closeDrawer();
    hidePeek();
    if (nav.stack[nav.index] !== path) {
      nav.stack = nav.stack.slice(0, nav.index + 1);
      nav.stack.push(path);
      nav.index = nav.stack.length - 1;
    }
    v.hidden = false;
    v.scrollTop = 0;
    syncButton(true);
    var art = document.getElementById("doc-article");
    art.textContent = "";
    art.appendChild(document.createElement("p")).className = "doc-none";
    art.firstChild.textContent = "Reading " + label(path) + "…";
    art.hidden = false;
    document.getElementById("doc-edit").hidden = true;
    confirmAsk = null;
    cancelNew();
    restoredFor = "";
    var pathEl = document.getElementById("doc-path");
    pathEl.textContent = path;
    pathEl.title = path;
    document.getElementById("doc-facts").textContent = "";
    syncPins();
    fetch(docURL(path), { headers: { Accept: "application/json" } }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw { status: r.status, text: t }; });
      return r.json();
    }).then(function (d) { show(d, opts.frag); }).catch(function (e) {
      if (e && e.status === 404) {
        // a draft of a file master does not hold: a new file, opened in
        // the editor for its first save to create, its draft seeded with
        // the name the member gave
        if (!draftOf(path) && seedTitle) {
          keepDraft(path, "# " + seedTitle + "\n\n");
          seedTitle = "";
          restoredFor = "";
        }
        if (draftOf(path)) {
          versions[path] = null;
          restoredFor = "";
          document.getElementById("doc-article").textContent = "";
          enterEdit();
          return;
        }
      }
      failDoc(e && e.text || "", e && e.status);
    });
    if (opts.push !== false) history.pushState({ doc: path }, "", urlPath(path));
    remember(path);
  }
  function closeDoc(push) {
    var v = view();
    if (!visible()) return;
    keepScroll();
    hidePeek();
    closeRecent();
    cancelNew();
    confirmAsk = null;
    v.hidden = true;
    syncButton(false);
    if (push !== false) history.pushState({ doc: null }, "", urlPath(null));
    var b = document.getElementById("documents");
    if (b) b.focus();
  }
  function openFromURL() {
    if (location.hash.indexOf("#card=") === 0 || visible()) return;
    var path = docParam();
    if (path) openDoc(path, { push: false });
  }

  // the Documents button: where you left it, else the roadmap
  document.addEventListener("click", function (e) {
    if (!e.target.closest || !e.target.closest("#documents")) return;
    openDoc(nav.stack[nav.index] || load("recent", [])[0] || "docs/roadmap.md");
  });
  // a document's own links: a data-doc link opens in the view, a quiet
  // one goes nowhere
  document.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest("#doc-view a");
    if (!a) return;
    if (a.dataset.doc) {
      e.preventDefault();
      var frag = "", i = a.getAttribute("href").indexOf("#");
      if (i >= 0) frag = a.getAttribute("href").slice(i + 1);
      openDoc(a.dataset.doc, { frag: frag });
    } else if (a.classList.contains("doc-missing")) e.preventDefault();
  });
  // the head's buttons
  document.addEventListener("click", function (e) {
    if (!e.target.closest) return;
    if (e.target.closest("#doc-close")) closeDoc();
    else if (e.target.closest("#doc-back") && nav.index > 0) openDoc(nav.stack[nav.index - 1]);
    else if (e.target.closest("#doc-fwd") && nav.index < nav.stack.length - 1) openDoc(nav.stack[nav.index + 1]);
    else if (e.target.closest("#doc-wide")) {
      var v = view(), wide = !v.classList.contains("wide");
      v.classList.toggle("wide", wide);
      e.target.closest("#doc-wide").setAttribute("aria-pressed", String(wide));
      store("wide", wide);
    } else if (e.target.closest(".doc-recent-row")) openDoc(e.target.closest(".doc-recent-row").dataset.doc);
    else if (e.target.closest(".doc-pin")) openDoc(e.target.closest(".doc-pin").dataset.doc);
    else if (e.target.closest(".doc-pin-add")) pinOpen();
    else if (e.target.closest("#doc-new")) openNew();
    else if (e.target.closest("#doc-new-cancel")) cancelNew();
    else if (e.target.closest("#doc-mode-read")) readMode();
    else if (e.target.closest("#doc-mode-edit")) { confirmAsk = null; enterEdit(); }
  });
  // a new document: docs/<name>.md, its draft seeded with the name, the
  // first save creating it; a name master already holds opens the file
  // itself, read, nothing seeded
  var seedTitle = "";
  function openNew() {
    var form = document.getElementById("doc-new-form"), line = document.getElementById("doc-pathline");
    if (!form || !line || edit.mode !== "read") return;
    form.hidden = false;
    line.hidden = true;
    var i = document.getElementById("doc-name");
    i.value = "";
    i.focus();
  }
  function cancelNew() {
    var form = document.getElementById("doc-new-form"), line = document.getElementById("doc-pathline");
    if (form) form.hidden = true;
    if (line) line.hidden = false;
  }
  function createDoc(name) {
    name = name.trim().replace(/\.md$/i, "");
    var slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
    if (!slug) { document.getElementById("doc-name").focus(); return; }
    cancelNew();
    seedTitle = name.charAt(0).toUpperCase() + name.slice(1);
    openDoc("docs/" + slug + ".md");
  }
  document.addEventListener("submit", function (e) {
    if (!e.target.closest || e.target.id !== "doc-new-form") return;
    e.preventDefault();
    createDoc(document.getElementById("doc-name").value);
  });
  // Read: back to the rendered document, fetched again when the save
  // that landed is not rendered yet
  function readMode() {
    var path = current(), ver = versions[path];
    if (!path || !ver) return;
    confirmAsk = null;
    if (ver.html) setMode("read");
    else openDoc(path, { push: false });
  }
  // leave an unsaved new file: back, else closed; the draft stays, the
  // dot says so
  function leaveNew() {
    if (nav.index > 0) openDoc(nav.stack[nav.index - 1]);
    else closeDoc();
  }
  // the editor's row and the note's links: save, retry, cancel, discard
  // with its ask
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-act]");
    if (!b || !b.closest("#doc-view")) return;
    var path = current(), ver = versions[path];
    var act = b.dataset.act;
    if (act === "save" || act === "retry") {
      saveDoc(path);
      var ed = document.getElementById("doc-editor");
      if (ed && act === "retry") ed.focus({ preventScroll: true });
    } else if (act === "cancel") {
      confirmAsk = null;
      if (ver) readMode(); else leaveNew();
    } else if (act === "discard") {
      confirmAsk = { at: b.closest("#doc-edit-row") ? "row" : "note" };
      syncDoc();
      var no = document.querySelector('#doc-view [data-act="confirm-no"]');
      if (no) no.focus();
    } else if (act === "discard-yes") {
      dropDraft(path);
      saves[path] = {};
      confirmAsk = null;
      restoredFor = "";
      if (ver) readMode(); else leaveNew();
    } else if (act === "confirm-no") {
      confirmAsk = null;
      syncDoc();
    }
  });
  // the refusal's retry on the path line
  document.addEventListener("click", function (e) {
    var f = e.target.closest && e.target.closest("#doc-facts.doc-refused");
    if (!f || !current()) return;
    saveDoc(current());
  });
  // every keystroke is the draft; typing back the saved text drops it
  document.addEventListener("input", function (e) {
    if (e.target.id !== "doc-editor" || !current()) return;
    var path = current(), ver = versions[path];
    if (ver && ver.text !== null && e.target.value === ver.text) dropDraft(path);
    else keepDraft(path, e.target.value);
    var s = saves[path];
    if (s) s.landed = null;
    syncDoc();
  });
  // Ctrl+S in the editor saves; Escape leaves the ask or the new form,
  // and never closes the view from inside a field
  document.addEventListener("keydown", function (e) {
    if (e.target.id === "doc-editor" && (e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s") {
      e.preventDefault();
      saveDoc(current());
    }
  });
  function closeRecent() {
    var d = document.getElementById("doc-recent");
    if (d) d.open = false;
  }
  document.addEventListener("click", function (e) {
    if (!e.target.closest("#doc-recent")) closeRecent();
  });
  // a card takes the slot: the document hides while the drawer shows and
  // comes back when it closes
  window.addEventListener("hashchange", function () {
    if (location.hash.indexOf("#card=") !== 0 || !visible()) return;
    keepScroll();
    hidePeek();
    view().hidden = true;
    syncButton(false);
  });
  document.addEventListener("drawerclosed", openFromURL);
  window.addEventListener("popstate", function () {
    var path = docParam();
    if (path) openDoc(path, { push: false });
    else if (visible()) closeDoc(false);
  });
  // Escape: the Recent menu, the peek, the editor's ask and the new
  // form, the document — the keys list and the status band close first,
  // and from inside a field the view stays open, the draft with it
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    var keys = document.getElementById("keys"), panel = document.getElementById("status-panel");
    if (keys && !keys.hidden || panel && !panel.hidden) return;
    var d = e.target.closest && e.target.closest("#doc-recent");
    if (d && d.open) { d.open = false; e.stopPropagation(); return; }
    if (peekEl) { hidePeek(); e.stopPropagation(); return; }
    var field = e.target.closest && e.target.closest("#doc-editor, #doc-name");
    var form = document.getElementById("doc-new-form");
    if (confirmAsk) { confirmAsk = null; syncDoc(); e.stopPropagation(); return; }
    if (form && !form.hidden) { cancelNew(); e.stopPropagation(); return; }
    if (field) { e.stopPropagation(); return; }
    if (visible() && location.hash.indexOf("#card=") !== 0) { closeDoc(); e.stopPropagation(); }
  }, true);

  // the peek: 400ms of hover over a document's link shows its first
  // paragraph, never on a coarse pointer or a narrow screen
  function finePointer() {
    return window.matchMedia("(hover: hover) and (pointer: fine)").matches && window.innerWidth >= 900;
  }
  function hidePeek() {
    if (hoverTimer) { clearTimeout(hoverTimer); hoverTimer = null; }
    if (peekEl) { peekEl.remove(); peekEl = null; }
    peekFor = null;
  }
  document.addEventListener("mouseover", function (e) {
    var a = e.target.closest && e.target.closest("#doc-view a[data-doc]");
    if (!a || !finePointer() || peekFor === a) return;
    hidePeek();
    peekFor = a;
    hoverTimer = setTimeout(function () { showPeek(a); }, 400);
  });
  document.addEventListener("mouseout", function (e) {
    if (e.target.closest && e.target.closest("#doc-view a[data-doc]")) hidePeek();
  });
  window.addEventListener("scroll", hidePeek, true);
  function showPeek(a) {
    var path = a.dataset.doc;
    var go = function (text) {
      if (peekFor !== a || !a.isConnected || peekEl) return;
      peekEl = document.createElement("div");
      peekEl.className = "doc-peek";
      peekEl.textContent = text || "";
      document.body.appendChild(peekEl);
      var r = a.getBoundingClientRect();
      peekEl.style.top = Math.max(8, Math.min(r.bottom + 8, window.innerHeight - peekEl.offsetHeight - 8)) + "px";
      peekEl.style.left = Math.max(8, Math.min(r.left, window.innerWidth - peekEl.offsetWidth - 8)) + "px";
    };
    if (peeks[path] !== undefined) return go(peeks[path]);
    fetch(docURL(path) + "?peek=1").then(function (r) { return r.ok ? r.text() : ""; }).then(function (t) {
      peeks[path] = t;
      go(t);
    });
  }

  // the board's swaps bring a fresh Documents button: say what it opens
  document.addEventListener("htmx:afterSettle", function () { syncButton(visible()); });
})();
