// The board subscribes to the event stream instead of polling; skip an
// htmx request while a form on the board is being filled in (text typed,
// a choice changed, or a field focused) so the swap does not wipe it.
document.addEventListener("htmx:beforeRequest", function (e) {
  var board = e.detail.elt;
  if (board.id !== "board") return;
  if (board.querySelector("details[open]")) {
    e.preventDefault();
    return;
  }
  if (board.contains(document.activeElement) && document.activeElement.matches("input, textarea, select")) {
    e.preventDefault();
    return;
  }
  var fields = board.querySelectorAll("input, textarea");
  for (var i = 0; i < fields.length; i++) {
    var f = fields[i];
    if (f.type === "hidden") continue;
    if ((f.type === "radio" || f.type === "checkbox") ? f.checked !== f.defaultChecked : f.value !== f.defaultValue) {
      e.preventDefault();
      return;
    }
  }
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
  var card = e.target.closest && e.target.closest("[data-card]");
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
});
document.addEventListener("DOMContentLoaded", offerLanes);

// Theme toggle: dark is the default; the choice is remembered in localStorage.
(function () {
  var stored = localStorage.getItem("theme");
  var theme = stored === "light" ? "light" : "dark";
  document.documentElement.setAttribute("data-theme", theme);
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

// The board opens an EventSource on the project's stream; on each event it
// swaps the card the event names and its activity line. A project-level
// event (card_filed, project_merged) swaps the whole board. A failed
// stream falls back to the poll after 30 s without messages and says so in
// the footer.
(function () {
  var board = document.getElementById("board");
  if (!board) return;
  var project = board.dataset.project;
  if (!project) return;
  var url = "/projects/" + encodeURIComponent(project) + "/events/stream";
  var es = null;
  var fallbackTimer = null;
  var footer = document.querySelector("footer");
  function showFallback() {
    if (!footer) {
      footer = document.createElement("footer");
      document.body.appendChild(footer);
    }
    footer.textContent = "The event stream failed; the board polls every 3 s.";
  }
  function clearFallback() {
    if (footer) footer.textContent = "";
  }
  function startPoll() {
    board.setAttribute("hx-get", "/board" + (project ? "?project=" + encodeURIComponent(project) : ""));
    board.setAttribute("hx-trigger", "every 3s");
    board.setAttribute("hx-swap", "outerHTML");
    htmx.process(board);
  }
  function openStream() {
    es = new EventSource(url);
    var lastMessage = Date.now();
    es.addEventListener("event", function (e) {
      lastMessage = Date.now();
      clearFallback();
      try {
        var ev = JSON.parse(e.data);
        if (ev.kind === "card_filed" || ev.kind === "project_merged") {
          htmx.ajax("GET", "/board" + (project ? "?project=" + encodeURIComponent(project) : ""), {target: board, swap: "outerHTML"});
          return;
        }
        if (ev.task_id) {
          htmx.ajax("GET", "/cards/" + encodeURIComponent(ev.task_id), {target: "[data-card=\"" + CSS.escape(ev.task_id) + "\"]", swap: "outerHTML"});
        }
      } catch (err) {}
    });
    es.onerror = function () {
      if (Date.now() - lastMessage > 30000) {
        es.close();
        showFallback();
        startPoll();
      }
    };
  }
  openStream();
})();
