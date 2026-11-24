// time.js reads a screen's times in the browser's zone. The server writes
// each one as <time datetime data-layout> in Europe/Berlin, in a Go
// layout; localTime fills in the layout's tokens again in the browser's
// zone, named as en-GB names it (CET, BST), else as en-US does (EST), else
// by its GMT offset.
var localTime = (function () {
  var months = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];
  var parts = new Intl.DateTimeFormat("en-GB", {weekday: "short", year: "numeric", month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit",
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
    return layout.replace(/2006|January|Jan|Mon|MST|01|02|15|04|05|2/g, function (t) {
      switch (t) {
        case "2006": return p.year;
        case "Mon": return p.weekday;
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

// localTimes writes every <time data-layout> under root again in the
// browser's zone. A <time> with a title is an age, its moment the title.
function localTimes(root) {
  root.querySelectorAll("time[data-layout]").forEach(function (el) {
    var text = localTime(new Date(el.dateTime), el.dataset.layout);
    if (el.title) el.title = text;
    else el.textContent = text;
  });
}

// timeHTML is the <time> of d in layout, read in the browser's zone.
function timeHTML(d, layout) {
  return "<time datetime=\"" + d.toISOString() + "\" data-layout=\"" + layout + "\">" + localTime(d, layout) + "</time>";
}

// timed is html with the words of each of times in its text written as the
// <time> of its instant in its layout, read in the browser's zone
// (moment.go's timed): the longer words first; words in a tag or in code
// stay as they are written.
function timed(html, times) {
  if (!times || !times.length) return html;
  var by = {};
  times.forEach(function (t) { by[t.words] = t; });
  var said = new RegExp(Object.keys(by).sort(function (a, b) { return b.length - a.length; }).map(function (w) {
    return w.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }).join("|"), "g");
  var code = 0;
  return html.split(/(<[^>]*>)/).map(function (part, i) {
    if (i % 2) {
      if (/^<(code|pre)\b/i.test(part)) code++;
      else if (/^<\/(code|pre)\b/i.test(part) && code) code--;
      return part;
    }
    return code ? part : part.replace(said, function (w) { return timeHTML(new Date(by[w].at), by[w].layout); });
  }).join("");
}
