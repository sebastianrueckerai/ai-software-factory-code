// The board polls every 10 s; skip a poll while an answer is being written
// so the swap does not wipe it.
document.addEventListener("htmx:beforeRequest", function (e) {
  if (e.detail.elt.id !== "board") return;
  var fields = e.detail.elt.querySelectorAll("textarea, input[type=text]");
  for (var i = 0; i < fields.length; i++) {
    if (fields[i].value !== "" || fields[i] === document.activeElement) {
      e.preventDefault();
      return;
    }
  }
});
