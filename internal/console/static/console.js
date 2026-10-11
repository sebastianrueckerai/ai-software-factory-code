// busy: a form in el is being filled in (a field focused, text typed, a
// choice changed) or a card's expanded view in it is open, so a swap of el
// would wipe or close it; and while the card drawer is open, the board
// holds still beside it.
function busy(el) {
  var drawer = document.getElementById("drawer");
  if (drawer && !drawer.hidden) return true;
  if (el.querySelector("details[open]")) return true;
  if (el.contains(document.activeElement) && document.activeElement.matches("input, textarea, select")) return true;
  var fields = el.querySelectorAll("input, textarea");
  for (var i = 0; i < fields.length; i++) {
    var f = fields[i];
    if (f.type === "hidden") continue;
    if ((f.type === "radio" || f.type === "checkbox") ? f.checked !== f.defaultChecked : f.value !== f.defaultValue) return true;
  }
  return false;
}
// The board's own requests, its poll and the swaps the stream asks for,
// skip a busy board.
document.addEventListener("htmx:beforeRequest", function (e) {
  if (e.detail.elt.id === "board" && busy(e.detail.elt)) e.preventDefault();
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
  // the board's project and accent show at once
  if (e.target.id === "project-filter" || e.target.id === "accent-filter") e.target.form.submit();
});
document.addEventListener("DOMContentLoaded", offerLanes);

// Theme toggle: dark is the default; the choice is remembered in
// localStorage, which theme.js applies before the first paint.
(function () {
  var theme = document.documentElement.getAttribute("data-theme");
  var btn = document.getElementById("theme-toggle");
  if (!btn) return;
  function label() {
    btn.textContent = "Theme: " + theme;
    btn.setAttribute("aria-label", "Switch to the " + (theme === "dark" ? "light" : "dark") + " theme");
  }
  label();
  btn.addEventListener("click", function () {
    theme = theme === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", theme);
    localStorage.setItem("theme", theme);
    label();
  });
})();

// The board follows its project's event stream: an event swaps the cards
// it names from GET /cards/{id} at once, or the whole board when the card
// is not on it or its place changed, and card_filed, project_merged and a
// reconnect swap the board. Beside the stream the board polls every 30 s
// (its template's hx-trigger), so its minute counters tick and whatever
// the events do not carry is at most 30 s old. A board whose stream
// failed (closed, or in error for 30 s) polls every 3 s instead, which
// the footer says. A swap a busy card or board would wipe waits until it
// is no longer busy.
(function () {
  var polling = false, missed = {}, latest = {}, asked = 0;
  function board() { return document.getElementById("board"); }
  function poll() {
    var b = board();
    if (!polling || !b || b.getAttribute("hx-trigger") === "every 3s") return;
    b.setAttribute("hx-trigger", "every 3s");
    htmx.process(b);
  }
  // a swapped-in board comes with the 30 s poll only
  document.addEventListener("htmx:afterSettle", poll);
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
  setInterval(function () {
    var ids = Object.keys(missed);
    missed = {};
    ids.forEach(function (id) {
      if (id) swapCard(id);
      else swapBoard();
    });
  }, 3000);
  function follow(project) {
    var es = new EventSource("/projects/" + encodeURIComponent(project) + "/events/stream");
    var failing = null, opened = false;
    function fallBack() {
      es.close();
      document.getElementById("live").textContent = "The live stream failed: the board polls every 3 s.";
      polling = true;
      poll();
    }
    es.onopen = function () {
      clearTimeout(failing);
      failing = null;
      // the replay after a reconnect holds at most the newest 500 events
      if (opened) swapBoard();
      opened = true;
    };
    es.onerror = function () {
      if (es.readyState === EventSource.CLOSED) fallBack();
      else if (!failing) failing = setTimeout(fallBack, 30000);
    };
    es.addEventListener("event", function (m) {
      var e = JSON.parse(m.data);
      if (!e.task_id || e.kind === "card_filed" || e.kind === "project_merged") swapBoard();
      else swapCard(e.task_id);
    });
  }
  document.addEventListener("DOMContentLoaded", function () {
    var b = board();
    if (b && b.dataset.project) follow(b.dataset.project);
  });
})();

// The card drawer beside the board: #card=<id> in the URL opens it, on
// load too, and a card's title links there; Escape or its ✕ closes it.
// Its tabs and related cards load into it by hx-get, and the URL follows
// the card it shows.
function drawer() { return document.getElementById("drawer"); }
function showDrawer() {
  var d = drawer(), m = /^#card=(.+)$/.exec(location.hash);
  if (!d) return;
  document.body.classList.toggle("drawer-open", !!m);
  d.hidden = !m;
  if (!m) {
    d.textContent = "";
    return;
  }
  htmx.ajax("GET", "/cards/" + encodeURIComponent(decodeURIComponent(m[1])) + "/drawer", {target: "#drawer", swap: "innerHTML"}).then(function () {
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
document.addEventListener("htmx:afterSwap", function (e) {
  var shown = e.detail.target.id === "drawer" && e.detail.target.querySelector("[data-drawer]");
  if (shown && location.hash !== "#card=" + shown.dataset.drawer) history.replaceState(null, "", "#card=" + shown.dataset.drawer);
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
    if (e.key === "Escape") {
      if (keys() && !keys().hidden) showKeys(false);
      else closeDrawer();
      return;
    }
    if (e.target.closest("input, textarea, select, [contenteditable]")) return;
    if (e.key === "w") {
      e.preventDefault();
      document.getElementById("view-toggle").click();
    } else if (e.key === "?") {
      e.preventDefault();
      showKeys(true);
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
