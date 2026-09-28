// The board polls every 10 s; skip a poll while a form on it is being
// filled in (text typed, a choice changed, or a field focused) so the swap
// does not wipe it.
document.addEventListener("htmx:beforeRequest", function (e) {
  var board = e.detail.elt;
  if (board.id !== "board") return;
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
