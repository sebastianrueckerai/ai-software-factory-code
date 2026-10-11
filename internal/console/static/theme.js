// Loaded without defer, so the page paints as the human chose at once:
// dark by default, light once chosen with the toggle; and the board in
// the view last chosen, its card drawer kept.
document.documentElement.setAttribute("data-theme", localStorage.getItem("theme") === "light" ? "light" : "dark");
(function () {
  var q = new URLSearchParams(location.search);
  if (location.pathname !== "/" || q.has("view") || localStorage.getItem("view") !== "wide") return;
  q.set("view", "wide");
  location.replace("/?" + q + location.hash);
})();
