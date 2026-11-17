// Loaded without defer, so the page paints as the human chose at once:
// dark by default, light once chosen with the toggle.
document.documentElement.setAttribute("data-theme", localStorage.getItem("theme") === "light" ? "light" : "dark");
