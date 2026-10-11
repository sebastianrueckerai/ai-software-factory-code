// Loaded without defer, so the page paints in the human's theme at once:
// dark by default, light once chosen with the toggle.
document.documentElement.setAttribute("data-theme", localStorage.getItem("theme") === "light" ? "light" : "dark");
