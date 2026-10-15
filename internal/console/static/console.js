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
