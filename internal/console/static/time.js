// time.js reads a screen's times in the browser's zone. The server writes
// each one as <time datetime data-layout> in Europe/Berlin, in a Go
// layout; localTime fills in the layout's tokens again in the browser's
// zone, named as en-GB names it (CET, BST), else as en-US does (EST), else
// by its GMT offset.
var localTime = (function () {
  var months = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];
  var parts = new Intl.DateTimeFormat("en-GB", {year: "numeric", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit",
    second: "2-digit", hourCycle: "h23"});
  var names = ["en-GB", "en-US"].map(function (locale) { return new Intl.DateTimeFormat(locale, {timeZoneName: "short"}); });
  function zone(d) {
    var name = "";
    names.some(function (f) {
      name = f.formatToParts(d).filter(function (p) { return p.type === "timeZoneName"; })[0].value;
      return !/^GMT[+-]/.test(name);
    });
    return name;
  }
  return function (d, layout) {
    var p = {}, two = function (n) { return ("0" + n).slice(-2); };
    parts.formatToParts(d).forEach(function (x) { p[x.type] = x.value; });
    return layout.replace(/2006|January|Jan|MST|01|02|15|04|05|2/g, function (t) {
      switch (t) {
        case "2006": return p.year;
        case "January": return months[p.month - 1];
        case "Jan": return months[p.month - 1].slice(0, 3);
        case "MST": return zone(d);
        case "01": return two(p.month);
        case "02": return two(p.day);
        case "15": return two(p.hour);
        case "04": return two(p.minute);
        case "05": return two(p.second);
        default: return String(+p.day);
      }
    });
  };
})();

// timeHTML is the <time> of d in layout, read in the browser's zone.
function timeHTML(d, layout) {
  return "<time datetime=\"" + d.toISOString() + "\" data-layout=\"" + layout + "\">" + localTime(d, layout) + "</time>";
}
