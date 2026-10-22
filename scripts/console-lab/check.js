// check.js is the structural check of the console design round: against
// the lab (run.sh starts it) it reads every scenario in the dark theme
// and fails on a control of design/inventory.md that is missing, or a
// mechanic that does not work: the drawer docked at the right edge and
// never over the chat, Escape closing it, Do it becoming an applied line,
// the routines' ghosts at the foot of their lanes with their control and
// drawer and their period, the find narrowing the board as you type, a
// card's press moving the card at once and taking one press, the chat
// above the board at 390 px, no horizontal scroll at 1440, 960 or 390,
// typing in the chat, nor on the pages of tables at 390.
// Exit 1 and one line per failure.
const crypto = require("crypto");
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");

const base = process.env.BASE || "http://127.0.0.1:8090";
const labDir = process.env.CONSOLE_LAB_DIR || "/tmp/claude-1000/console-lab";
const [name, value] = fs.readFileSync(path.join(labDir, "cookie.txt"), "utf8").trim().split(/=(.*)/s);

// contrastEl are the C14 parts' text, measured on the rendered page in
// both themes (design/rounds/02-multi-chat/check.js)
const contrastEl = ".trow .ttitle, .trow .tat, .trow.arch .ttitle, .tarch-l, .restore, .badge, .thint, .decider, .esc-l, .answer-l, .answer-or, .msg.settled .fold, " +
  ".cmd, .palette button span, .palette button b, #level-btn, .lvlpop b, .lvlpop .lw, .lvlpop .lp-head span, .msg.sum .said, " +
  ".msg.poke .who, .thead .ttl, .thr span, .tacts button, .overlay-box p, .overlay-box h2, .answer button, .arch-note, .thread-link, " +
  ".doc-path, .doc-facts, .doc-refused, .doc-dot, .doc-base, .doc-note, .doc-confirm-q, .doc-new-l, .doc-discard, #doc-new, " +
  ".doc-recent > summary, .doc-mode button, .doc-recent-label, .doc-none, #doc-article a, .doc-pins-l, .doc-pin, .doc-pin-add, #doc-forgejo";

// ghostEl are the routines' text (C16), measured as contrastEl is
const ghostEl = ".card.ghost .ref, .card.ghost .ttl, .card.ghost .line2, .card.ghost .held, .card.ghost .run, .card.ghost .acts button, " +
  ".chip.routine, .cell.routine";

// notifyEl are the messenger panel's text (roadmap 145, C19), measured
// as contrastEl is
const notifyEl = ".notify th[scope=row], .notify td, .notify .dim, .notify .addr, .notify caption, .notify td.act a, .notify td.act button, .notify select";

// metricsEl are the metrics page's text (round 13, C30), measured as
// contrastEl is
const metricsEl = ".mhead h2, .mintro, .mwin a, .msec > h3, .mname, .mval, .mmove";
const metricsPath = "/projects/ai-software-factory-code/metrics";

// labID is issue's task id in the lab, as lab_test.go's labID makes it.
const labID = (issue) => "t-" + crypto.createHash("sha256").update("lab " + issue).digest("hex").slice(0, 12);

// mustHave are the inventory's controls and data points as the lab's
// scenarios word them; a missing word is a missing control.
const mustHave = {
  threads: ["/compact", "/new", "mark · thread · last", "Why is the session store card waiting?", "Give the costs page a from and a to date.",
    "Plan the members page", "Export the project to GitHub", "a turn in flight", "1 new", "New"],
  escalation: ["Answer:", "or in your words below", "needs you", "autonomous level · started by the agent", "Which store keeps the nightly board snapshots?"],
  poke: ["Inbox", "Why is the session store card waiting?", "1 needs you · 1 new", "archived", "restore", "Archive", "Delete…"],
  compacted: ["4 earlier messages, compacted", "summary", "#11 stays in the backlog until the store is settled.",
    "Why is the session store card waiting?"],
  wake: ["wake", "#603: Which store keeps the nightly board snapshots?"],
  assisted: ["Inbox", "Board", "autonomy", "assisted", "Moved #8 to lane ready",
    "#7", "#8", "#9", "Speed up the find box", "Fix the login redirect", "Trim the roll notes"],

  busy: ["Factory console", "New task", "Settings", "Resources", "Costs", "Policy", "Notifications", "Log out", "sebastian", "Theme",
    "waiting on you", "3 working", "3 need you", "€61.40", "€120.00", "GitHub", "configuration", "version 4",
    "Needs you", "Queued", "In progress", "Done",
    "Merge", "PR #612", "high", "migration", "Approve", "Deny", "Split", "requesting changes", "rejecting",
    "Which store keeps the nightly board snapshots", "Postgres", "SQLite", "Ask the author", "answer on the issue", "Close",
    "How far back does the feed page by default?", "One day", "One week",
    "#598", "#616", "#617", "waiting on #603, #611", "#533",
    "#606", "3/4 checks", "€1.72", "near its budget", "#611", "#614", "#601", "PR #605",
    "#531", "#596", "#600", "#597", "failed", "#590", "closed", "#535", "Retry",
    "project agent", "Start a new thread", "What is #606 stuck on", "Do it", "Skip", "Send",
    "File roadmap 300 as a card", "One card, or one per deliverable", "One card",
    "Refused: The ready check refuses the filing", "0 acceptance tests; want 1 to 10",
    "acceptance the chat shows the documents a member sends; the agent reads them by tool",
    "The failing run is img1", "analyze_image", "In #606's session", "A fresh session",
    "Wide view", "?", "Find on the board", "Find: a word, #612, plan:558",
    "Order and show", "Order", "Show", "newest first", "oldest first", "next up", "by stage", "by spend", "blocked only", "exceptional only", "failed only",
    "↻ routines", "within", "Weekly bump", "Template pass", "Dependency update", "Audit", "every Monday 06:00", "after 6 more merges",
    "Skip once", "Run now", "#779"],
  idle: ["idle", "0 working", "0 need you", "€144.16", "#455", "#178", "Done", "#618", "#597", "failed"],
  fire: ["#610", "Grant one session with this answer", "Fund", "real problem", "nothing for over", "2 waiting on the provider",
    "#620", "€1.95", "#615", "#621", "#604", "#609", "failed", "Resource request", "cx33", "load test of the event stream",
    "beyond the envelope", "#623", "waiting on #610", "#602"],
  quiet: ["3 working", "0 need you", "€4.10", "#624", "#622", "#621", "#620", "done"],

  metrics: ["Delivery", "Stability", "Cost", "Autonomy", "The factory itself", "deploys / week", "lead time, push to live",
    "spend by model", "served by", "7 days", "28 days", "90 days"],
};

async function seed(s) {
  const resp = await fetch(base + "/lab/scenario/" + s);
  if (resp.status !== 204) throw new Error("GET /lab/scenario/" + s + ": " + resp.status);
  return resp.headers.get("x-lab-drawer") || "";
}

// open is a fresh page at viewport in the dark theme; its thread table is
// unfolded, as for a member who unfolded it once, unless folded is set,
// which leaves the table at its default (folded to its marks).
async function open(browser, viewport, folded) {
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: 1 });
  await ctx.addCookies([{ name, value, domain: new URL(base).hostname, path: "/", secure: true, httpOnly: true, sameSite: "Lax" }]);
  await ctx.addInitScript(() => localStorage.setItem("theme", "dark"));
  if (!folded) await ctx.addInitScript(() => { if (localStorage.getItem("chat-fold:sebastian") === null) localStorage.setItem("chat-fold:sebastian", "0"); });
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  page.on("response", (r) => { if (r.url().startsWith(base) && r.status() >= 400) errors.push(r.url() + " " + r.status()); });
  return { ctx, page, errors };
}

// sideways is how far page scrolls sideways, in px. The browser keeps its
// scrollbars (the launch drops --hide-scrollbars), so a box 1 px past the
// edge shows as the scrollbar a desktop browser draws for it.
const sideways = (page) => page.evaluate(() => Math.max(0, document.documentElement.scrollWidth - document.documentElement.clientWidth));

// findAsYouType types word into the find of the busy board under query
// without Enter and checks that after the pause the board, and nothing
// else, shows only the cards that say word, the URL holds q=word beside
// the board's other fields in place of the board's history entry, and the
// box keeps its focus, its text and its caret; that emptying the box makes
// the board whole again; that Back goes to the page before the board and
// Forward returns to the find, the box holding word; and that the × shows
// while the box holds text and clears the find and the URL.
async function findAsYouType(p, query, word, label, fails) {
  const page = p.page, cards = "#board [data-card]";
  await page.goto(base + "/costs", { waitUntil: "load" });
  await page.goto(base + "/" + query, { waitUntil: "load" });
  await page.waitForTimeout(600);
  const whole = await page.locator(cards).count(), kept = new URL(page.url()).searchParams, depth = await page.evaluate(() => history.length);
  if (await page.locator("#find .find-x").isVisible()) fails.push(label + ": the × shows while the box is empty");
  await page.evaluate(() => { window.labPage = true; window.labBox = document.getElementById("q"); });
  await page.locator("#q").click();
  await page.keyboard.type(word, { delay: 40 });
  await page.waitForTimeout(1200);
  const at = new URL(page.url()).searchParams;
  if (at.get("q") !== word) fails.push(label + ": the URL holds q=" + at.get("q") + ", want " + word + " (" + page.url() + ")");
  for (const [k, v] of kept) if (at.get(k) !== v) fails.push(label + ": the find drops " + k + "=" + v + " (" + page.url() + ")");
  if ([...at.keys()].some((k) => k.endsWith("-shown"))) fails.push(label + ": the URL carries a column's shown count (" + page.url() + ")");
  const texts = await page.locator(cards).allInnerTexts();
  if (!texts.length || texts.length >= whole) fails.push(label + ": " + word + " shows " + texts.length + " of the board's " + whole + " cards");
  const off = texts.filter((t) => !t.toLowerCase().includes(word));
  if (off.length) fails.push(label + ": " + word + " shows cards that do not say it: " + off.map((t) => t.slice(0, 40)).join(" | "));
  const box = () => page.evaluate(() => {
    const q = document.getElementById("q");
    return { focus: document.activeElement === q, value: q.value, caret: q.selectionStart, same: window.labPage === true, box: window.labBox === q };
  });
  let b = await box();
  if (!b.same) fails.push(label + ": typing reloads the page");
  if (!b.box) fails.push(label + ": the swap replaces the find's box");
  if (!b.focus || b.value !== word || b.caret !== word.length) fails.push(label + ": after the swap the box has focus " + b.focus + ", text " + JSON.stringify(b.value) + ", caret " + b.caret);
  if (!(await page.locator("#find .find-x").isVisible())) fails.push(label + ": no × while the box holds text");
  await page.keyboard.press("Control+A");
  await page.keyboard.press("Backspace");
  await page.waitForTimeout(1200);
  b = await box();
  if (new URL(page.url()).searchParams.get("q")) fails.push(label + ": the emptied box leaves q in the URL (" + page.url() + ")");
  if ((await page.locator(cards).count()) !== whole) fails.push(label + ": the emptied box shows " + (await page.locator(cards).count()) + " cards, not the whole " + whole);
  if (!b.focus || b.value !== "") fails.push(label + ": the emptied box has focus " + b.focus + ", text " + JSON.stringify(b.value));
  if (await page.locator("#find .find-x").isVisible()) fails.push(label + ": the × stays on the emptied box");
  await page.keyboard.type(word, { delay: 40 });
  await page.waitForTimeout(1200);
  const entries = await page.evaluate(() => history.length);
  if (entries !== depth) fails.push(label + ": three finds leave " + entries + " history entries, want the board's " + depth);
  await page.goBack({ waitUntil: "load" });
  await page.waitForTimeout(600);
  if (new URL(page.url()).pathname !== "/costs") fails.push(label + ": Back after the find goes to " + page.url() + ", not the page before the board");
  await page.goForward({ waitUntil: "load" });
  await page.waitForTimeout(600);
  const fwd = { q: new URL(page.url()).searchParams.get("q"), box: await page.locator("#q").inputValue(), cards: await page.locator(cards).count() };
  if (fwd.q !== word || fwd.box !== word || fwd.cards !== texts.length) {
    fails.push(label + ": Forward reads q=" + fwd.q + ", the box " + JSON.stringify(fwd.box) + " and " + fwd.cards + " cards, want " + word + " and " + texts.length);
  }
  if (!(await page.locator("#find .find-x").isVisible())) return fails.push(label + ": no × to clear the find after Forward");
  await page.locator("#find .find-x").click();
  await page.waitForLoadState("load");
  await page.waitForTimeout(600);
  if (new URL(page.url()).searchParams.has("q")) fails.push(label + ": the × leaves q in the URL (" + page.url() + ")");
  if ((await page.locator(cards).count()) !== whole || (await page.locator("#q").inputValue()) !== "") fails.push(label + ": the × does not make the board whole again");
}

// viewChange: the agent's view change (282) holds for the page as a
// click does. The event's query pushes the URL beside the open doc=,
// reads the board under it, leaves one more history entry and puts its
// q in the find's box; Back reads the entry's board again; the card
// opens the drawer and the tab lands once the drawer is in; reset
// closes the drawer and clears the URL's controls; the panel and the
// theme work their own controls.
async function viewChange(p, query, fails) {
  const page = p.page, label = "view change", cards = "#board [data-card]";
  await page.goto(base + "/" + query, { waitUntil: "load" });
  await page.waitForTimeout(600);
  const before = await page.evaluate(() => history.length);
  await page.evaluate(() => document.dispatchEvent(new CustomEvent("view",
    {detail: {query: "?project=ai-software-factory-code&queued=oldest&q=export"}})));
  await page.waitForTimeout(900);
  const at = new URL(page.url()).searchParams;
  if (at.get("q") !== "export" || at.get("queued") !== "oldest" || !at.get("project")) fails.push(label + ": the URL holds " + page.url());
  const words = await page.locator("#q").inputValue();
  if (words !== "export") fails.push(label + ": the find's box holds " + JSON.stringify(words) + ", not the event's q");
  if ((await page.evaluate(() => history.length)) !== before + 1) fails.push(label + ": the change leaves no history entry");
  const shown = await page.locator(cards).allInnerTexts();
  if (!shown.length || shown.some((t) => !t.toLowerCase().includes("export"))) fails.push(label + ": the board shows " + shown.length + " cards that do not say export");
  await page.goBack({ waitUntil: "load" });
  await page.waitForTimeout(600);
  if (new URL(page.url()).searchParams.get("q")) fails.push(label + ": Back leaves q in the URL (" + page.url() + ")");
  if ((await page.locator(cards).count()) < shown.length + 1) fails.push(label + ": Back does not read the entry's board");
  await page.locator("#status-line > summary").click();
  await page.waitForTimeout(200);
  if (!(await page.locator("#status-panel").isVisible())) fails.push(label + ": the strip does not open the panel");
  await page.locator("#status-line > summary").click();
  await page.waitForTimeout(200);
  if (!(await page.locator("#status-panel").isHidden())) fails.push(label + ": the strip does not close the panel");
  const r11 = await page.evaluate(() => ({
    chips: document.querySelectorAll("#status-panel .chiprow .lchip").length,
    glyph: !!document.querySelector("#chat-full.wctl svg"),
    edge: !!document.querySelector("#screen > .edge-tab"),
    bar: !!document.querySelector("#chat-form > .growbar")
  }));
  if (r11.chips < 1) fails.push(label + ": the panel holds no lamp chips");
  if (!r11.glyph) fails.push(label + ": the chat head has no window glyph");
  if (!r11.edge) fails.push(label + ": the screen holds no edge tab");
  if (!r11.bar) fails.push(label + ": the composer holds no drag seam");
  await page.hover("#intake-btn");
  if ((await page.evaluate(() => getComputedStyle(document.getElementById("intake-btn"), "::after").content)) === "none") fails.push(label + ": the quiet controls paint no tooltip");
  await page.click("#chat-full");
  await page.waitForTimeout(150);
  const full = await page.evaluate(() => ({
    on: document.getElementById("screen").classList.contains("chat-only"),
    col: getComputedStyle(document.querySelector("#chat-full .col")).display !== "none",
    edge: getComputedStyle(document.querySelector(".edge-tab")).display !== "none"
  }));
  if (!full.on || !full.col || !full.edge) fails.push(label + ": the glyph does not hand the screen to the chat with the edge tab");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(150);
  if (await page.evaluate(() => document.getElementById("screen").classList.contains("chat-only"))) fails.push(label + ": Escape does not return the board");
  const h0 = await page.evaluate(() => document.getElementById("chat-input").getBoundingClientRect().height);
  const seam = await page.evaluate(() => { const r = document.querySelector("#chat-form > .growbar").getBoundingClientRect(); return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) }; });
  await page.mouse.move(seam.x, seam.y);
  await page.mouse.down();
  await page.mouse.move(seam.x, seam.y - 120, { steps: 6 });
  await page.mouse.up();
  const h1 = await page.evaluate(() => Math.round(document.getElementById("chat-input").getBoundingClientRect().height));
  if (h1 <= h0) fails.push(label + ": the drag seam does not size the input (" + h0 + " to " + h1 + ")");
  await page.evaluate(() => { const i = document.getElementById("chat-input"); i.style.height = ""; });
  await page.evaluate((card) => document.dispatchEvent(new CustomEvent("view",
    {detail: {query: "?project=ai-software-factory-code", card: card, tab: "chat", panel: "closed", theme: "light"}})), labID(616));
  await page.waitForTimeout(900);
  const card = new URL(page.url()).hash;
  if (card.indexOf("#card=") !== 0) fails.push(label + ": the drawer's hash is " + card);
  if (!(await page.locator("#drawer").isVisible())) fails.push(label + ": the drawer does not show");
  if (!(await page.locator('#drawer .tabs button[aria-selected="true"][hx-get*="tab=chat"]').first().isVisible())) fails.push(label + ": the tab does not land");
  if (await page.evaluate(() => document.documentElement.getAttribute("data-theme") || localStorage.getItem("theme")) !== "light") fails.push(label + ": the theme does not change");
  await page.evaluate(() => document.dispatchEvent(new CustomEvent("view", {detail: {query: "?project=ai-software-factory-code", card: ""}})));
  await page.waitForTimeout(600);
  if (await page.locator("#drawer").isVisible()) fails.push(label + ": reset does not close the drawer");
  if (new URL(page.url()).searchParams.toString() !== "project=ai-software-factory-code") fails.push(label + ": reset leaves " + new URL(page.url()).searchParams);
}

// lateRead: on the busy board a read of the board goes out and its answer
// is held 1.5 s, a find answers first, then the read answers. The read is
// dropped and its promise settles; the box stays in the board with its
// focus and text, and the board is the find's.
async function lateRead(page, label, fails) {
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForTimeout(600);
  let held = false;
  const slow = (u) => u.pathname === "/board" && !u.searchParams.get("q");
  const hold = async (r) => {
    if (held) return r.continue();
    held = true;
    await new Promise((ok) => setTimeout(ok, 1500));
    await r.continue();
  };
  await page.route(slow, hold);
  await page.evaluate(() => {
    window.labRead = "pending";
    htmx.ajax("GET", "/board" + document.getElementById("board").dataset.query, { source: "#board", target: "#board", swap: "outerHTML" })
      .then(() => { window.labRead = "settled"; }, () => { window.labRead = "failed"; });
  });
  await page.locator("#q").click();
  await page.keyboard.type("export", { delay: 40 });
  await page.waitForTimeout(2500);
  await page.unroute(slow, hold);
  const after = await page.evaluate(() => {
    const q = document.getElementById("q"), b = document.getElementById("board");
    return { inBoard: !!q && b.contains(q), box: q && q.value, focus: !!q && document.activeElement === q, query: b.dataset.query, read: window.labRead,
      off: Array.from(b.querySelectorAll("[data-card]")).filter((k) => !k.innerText.toLowerCase().includes("export")).length };
  });
  if (!after.inBoard || after.box !== "export" || !after.focus) {
    fails.push(label + ": after a late read the box is " + (after.inBoard ? "in the board" : "gone") + ", text " + JSON.stringify(after.box) + ", focus " + after.focus);
  }
  if (after.query.indexOf("q=export") < 0 || after.off) fails.push(label + ": after a late read the board's query is " + after.query + ", " + after.off + " cards do not say export");
  if (after.read !== "settled") fails.push(label + ": the late read's promise is " + after.read);
}

// cardPress presses Postgres on the busy board's escalation (603), its
// answer held 1.2 s, and checks C31: before the answer is in the card
// stands at the top of In progress, busy, no button of it live, and a
// second press on it sends nothing; a refusal puts it back in Needs you
// where it stood, live, the refusal in the flash. Pressed again in its
// drawer and accepted while the factory's board still shows it waiting,
// it keeps its new place and its lock, the drawer's open need locked too,
// through the answer's board; a board that shows it out of Needs you lets
// it go.
async function cardPress(page, fails) {
  const id = labID(603), sel = '#board .cards > [data-card="' + id + '"]';
  await page.goto(base + "/", { waitUntil: "load" });
  await page.waitForTimeout(600);
  const where = () => page.evaluate((s) => {
    const k = document.querySelector(s), f = document.getElementById("flash");
    return k && { col: k.closest(".column").dataset.holds, at: Array.prototype.indexOf.call(k.parentNode.children, k), busy: k.getAttribute("aria-busy"),
      live: Array.from(k.querySelectorAll("button")).filter((b) => !b.disabled).length, flash: f.hidden ? "" : f.textContent };
  }, sel);
  const before = await where();
  if (!before || before.col.indexOf("waiting-human") < 0 || !before.live) return fails.push("card press: #603 is not a live card in Needs you: " + JSON.stringify(before));
  let posts = 0, answer;
  const answers = (u) => u.pathname === "/tasks/" + id + "/answer";
  await page.route(answers, async (r) => {
    posts++;
    await new Promise((ok) => setTimeout(ok, 1200));
    await answer(r);
  });
  answer = (r) => r.fulfill({ status: 409, contentType: "text/plain", body: "The lab refuses the answer." });
  await page.locator(sel + ' button:text-is("Postgres")').click();
  const now = await where();
  if (!now || now.col.indexOf("running") < 0 || now.at !== 0 || now.busy !== "true" || now.live) {
    fails.push("card press: right after the press the card stands " + JSON.stringify(now) + ", want the top of In progress, busy, no live button");
  }
  await page.evaluate((s) => document.querySelectorAll(s + ' form[action$="/answer"]')[1].requestSubmit(), sel);
  await page.waitForTimeout(200);
  if (posts !== 1) fails.push("card press: two presses on the card sent " + posts + " answers");
  await page.waitForTimeout(1400);
  const refused = await where();
  if (!refused || refused.col !== before.col || refused.at !== before.at || refused.busy || refused.live !== before.live || refused.flash !== "The lab refuses the answer.") {
    fails.push("card press: after the refusal the card stands " + JSON.stringify(refused) + ", want back where it stood " + JSON.stringify(before) + " and the refusal in the flash");
  }
  const query = await page.evaluate(() => document.getElementById("board").dataset.query);
  const waiting = await (await page.request.get(base + "/board" + query, { headers: { "HX-Request": "true" } })).text();
  answer = (r) => r.fulfill({ status: 200, contentType: "text/html", body: waiting });
  await page.evaluate((k) => { location.hash = "#card=" + k; }, id);
  await page.waitForSelector("#drawer .open-need button");
  posts = 0;
  await page.locator('#drawer .open-need button:text-is("Postgres")').click();
  await page.waitForTimeout(1800);
  const kept = await where(), drawerBusy = await page.evaluate(() => { const o = document.querySelector("#drawer .open-need"); return o && o.getAttribute("aria-busy"); });
  if (posts !== 1 || !kept || kept.col.indexOf("running") < 0 || kept.busy !== "true" || kept.live || drawerBusy !== "true") {
    fails.push("card press: accepted from the drawer while the factory's board shows the card waiting, " + posts + " answers sent, the card " + JSON.stringify(kept) +
      ", the drawer's open need busy " + drawerBusy + ", want one answer, the card in In progress and both locked");
  }
  await page.unroute(answers);
  await page.route((u) => u.pathname === "/board", (r) => r.fulfill({ status: 200, contentType: "text/html", body: waiting.split('data-card="' + id + '"').join('data-card="t-moved"') }));
  await page.evaluate(() => htmx.ajax("GET", "/board" + document.getElementById("board").dataset.query, { source: "#board", target: "#board", swap: "outerHTML" }));
  await page.waitForTimeout(800);
  const gone = await page.evaluate((s) => ({ card: !!document.querySelector(s), busy: document.querySelectorAll("#board [aria-busy], #board [data-sent]").length }), sel);
  if (gone.card || gone.busy) fails.push("card press: a board that shows the card out of Needs you leaves " + JSON.stringify(gone) + ", want the card let go");
}

async function text(page) {
  // what a disclosure hides (the deny form, the own-answer box, the status panel) counts once opened
  await page.evaluate(() => document.querySelectorAll('button.deny[aria-expanded="false"], button.own-toggle[aria-expanded="false"]').forEach((b) => b.click()));
  await page.evaluate(() => document.querySelectorAll("details.col-menu, #status-line").forEach((d) => { d.open = true; }));
  return page.evaluate(() => document.body.innerText + " " + Array.from(document.querySelectorAll("[aria-label],[title],[placeholder],[data-tip]"))
    .map((e) => (e.getAttribute("aria-label") || "") + " " + (e.getAttribute("title") || "") + " " + (e.getAttribute("placeholder") || "") + " " + (e.getAttribute("data-tip") || "")).join(" "));
}

(async () => {
  const browser = await chromium.launch({ ignoreDefaultArgs: ["--hide-scrollbars"] });
  const fails = [];
  for (const [sc, words] of Object.entries(mustHave)) {
    const drawer = await seed(sc);
    const p = await open(browser, { width: 1440, height: 900 });
    await p.page.goto(base + (sc === "metrics" ? metricsPath : "/"), { waitUntil: "load" });
    await p.page.waitForTimeout(600);
    // the status in detail opens on its chevron: its words count too
    const xp = p.page.locator("#status-toggle");
    if (await xp.count()) { await xp.click(); await p.page.waitForTimeout(200); }
    const low = (await text(p.page)).toLowerCase();
    if (await xp.count()) { await p.page.keyboard.press("Escape"); await p.page.waitForTimeout(200); }
    for (const w of words) if (!low.includes(w.toLowerCase())) fails.push(sc + ': missing "' + w + '"');
    if (sc === "metrics") {
      // the deck's rows fit the desk and the phone, no sideways scroll
      const sw = await sideways(p.page);
      if (sw) fails.push("metrics: horizontal page scroll at 1440 (" + sw + " px)");
      const mp = await open(browser, { width: 390, height: 844 });
      await mp.page.goto(base + metricsPath, { waitUntil: "load" });
      await mp.page.waitForTimeout(400);
      const psw = await sideways(mp.page);
      if (psw) fails.push("metrics: horizontal scroll at 390 (" + psw + " px)");
      if (mp.errors.length) fails.push("metrics phone: " + mp.errors[0]);
      await mp.ctx.close();
    }
    if (sc === "quiet") {
      const merged = p.page.locator('#board [data-card="' + labID(623) + '"]');
      const word = merged.locator(".line2 .st a");
      if ((await word.count()) !== 1 || (await word.innerText()).trim() !== "blocked by roll #626") fails.push("quiet: the merged card's face does not read the linked word blocked by roll #626");
      else if ((await word.getAttribute("href")) !== "#card=" + labID(626)) fails.push("quiet: the roll word links " + (await word.getAttribute("href")) + ", not the roll card's drawer");
      else {
        await word.click();
        await p.page.waitForTimeout(600);
        if (!(await p.page.locator("#drawer").innerText()).includes("roll 5e1f0a2")) fails.push("quiet: a click on the roll word does not open the roll card's drawer");
      }
      const rollCol = await p.page.locator('#board section:has(h3:has-text("In progress")) [data-card="' + labID(626) + '"]').count();
      if (!rollCol) fails.push("quiet: the roll card is not in the In progress column");
    }
    if (sc === "busy") {
      // the theme selector (C15): the sun sets light and is pressed, a second
      // click on it changes nothing, the moon sets dark again
      const themeIs = async (t) => (await p.page.evaluate(() => document.documentElement.dataset.theme)) === t &&
        (await p.page.locator('#theme-switch [data-theme-set="' + t + '"]').getAttribute("aria-pressed")) === "true";
      for (const [t, why] of [["light", "the sun does not set the light theme"], ["light", "a click on the sun in force changes the theme"], ["dark", "the moon does not set the dark theme"]]) {
        await p.page.locator('#theme-switch [data-theme-set="' + t + '"]').click();
        await p.page.waitForTimeout(200);
        if (!(await themeIs(t))) fails.push("busy: " + why);
      }
      const sw = await sideways(p.page);
      if (sw) fails.push("busy: horizontal page scroll at 1440 (" + sw + " px)");
      await p.page.locator("#chat textarea").first().click();
      // words that wrap the box, line breaks that grow it (Enter would send), one long word
      const typed = [() => p.page.keyboard.type("a message that runs long enough to wrap the composer ".repeat(4)),
        async () => { for (let i = 0; i < 6; i++) await p.page.keyboard.press("Shift+Enter"); },
        () => p.page.keyboard.type("https://example.com/" + "a".repeat(200))];
      for (const t of typed) {
        await t();
        const tsw = await sideways(p.page);
        if (tsw) fails.push("busy: horizontal page scroll at 1440 while typing in the chat (" + tsw + " px)");
      }
      await p.page.keyboard.press("Control+a");
      await p.page.keyboard.press("Delete");
      const card = p.page.locator('[data-card="' + drawer + '"]').first();
      if (!(await card.count())) fails.push("busy: no card " + drawer + " on the board");
      else {
        const chatBox = await p.page.locator("#chat").boundingBox();
        await card.click({ position: { x: 4, y: 4 } });
        await p.page.waitForTimeout(600);
        const d = p.page.locator("#drawer");
        if (!(await d.isVisible())) fails.push("busy: a click on the card does not open the drawer");
        else {
          const dt = await d.innerText();
          for (const w of ["Overview", "Session", "Activity", "Files"]) if (!dt.includes(w)) fails.push('drawer: missing the tab "' + w + '"');
          const tabs = await d.getByRole("tab").allInnerTexts();
          if (tabs[tabs.length - 1] !== "Files") fails.push("drawer: Files is not the last tab (" + tabs.join(", ") + ")");
          const box = await d.boundingBox();
          if (box && box.x + box.width < 1430) fails.push("drawer: not docked at the right edge (ends at " + Math.round(box.x + box.width) + ")");
          if (box && chatBox && box.x < chatBox.x + chatBox.width - 1) fails.push("drawer: over the chat (drawer from " + Math.round(box.x) + ", the chat ends at " + Math.round(chatBox.x + chatBox.width) + ")");
          await p.page.keyboard.press("Escape");
          await p.page.waitForTimeout(300);
          if (await d.isVisible()) fails.push("drawer: Escape does not close it");
        }
      }
      // the columns never scroll themselves (item 0a of roadmap 236): a column's body
      // scrolls down, only when its cards overflow, and never sideways
      const scroll = await p.page.evaluate(() => Array.from(document.querySelectorAll("#board .column")).map((c) => {
        const b = c.querySelector(":scope > .cards"), cs = b && getComputedStyle(b);
        return { col: c.scrollHeight > c.clientHeight + 2 || c.scrollWidth > c.clientWidth + 2, body: !b || b.scrollWidth > b.clientWidth, y: cs && cs.overflowY, x: cs && cs.overflowX };
      }));
      scroll.forEach((s, i) => {
        if (s.col) fails.push("busy: column " + i + " scrolls itself");
        if (s.body) fails.push("busy: column " + i + " has no body or its body scrolls sideways");
        if (s.y !== "auto" || s.x !== "hidden") fails.push("busy: column " + i + "'s body overflow is " + s.y + "/" + s.x + ", want auto/hidden");
      });
      // the ask's answer row (roadmap 300's round): the open ask offers
      // its presses between the log and the box, the member's own words
      // open below; a past ask shows its press as the member's row. It is
      // read before the Do it below: that press's row would settle the ask
      const answer = p.page.locator("#chat-answer");
      if (!(await answer.isVisible())) fails.push("chat: the open ask shows no answer row");
      else {
        const at = await answer.innerText();
        for (const w of ["Answer:", "In #606's session", "A fresh session", "or in your words below"]) if (!at.includes(w)) fails.push('chat: the answer row lacks "' + w + '"');
      }
      const doit = p.page.locator("#chat").getByRole("button", { name: "Do it", exact: true });
      if (!(await doit.count())) fails.push("chat: no Do it");
      else {
        await doit.last().click();
        await p.page.waitForTimeout(800);
        const t2 = await p.page.locator("#chat").innerText();
        if (!/applied/i.test(t2) || !/undo/i.test(t2)) fails.push("chat: Do it does not become an applied line with Undo");
      }
      // a pasted image rides the composer as a named chip (roadmap 300):
      // paste puts img1 above the box with its ×, the × removes it, and
      // nothing leaves the page until Send
      const chips = p.page.locator("#chat-images");
      await p.page.evaluate(() => new Promise((done) => {
        const c = document.createElement("canvas");
        c.width = 320; c.height = 200;
        const g = c.getContext("2d");
        g.fillStyle = "#8a9aa8"; g.fillRect(0, 0, 320, 200);
        c.toBlob((b) => {
          const dt = new DataTransfer();
          dt.items.add(new File([b], "shot.png", { type: "image/png" }));
          document.getElementById("chat-input").dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
          done();
        }, "image/png");
      }));
      await p.page.waitForTimeout(200);
      if (!(await chips.isVisible()) || !(await chips.innerText()).includes("img1")) fails.push("chat: a pasted image shows no img1 chip");
      else {
        const remove = chips.locator("[data-unimage]");
        if (!(await remove.count())) fails.push("chat: the image chip has no ×");
        else {
          await remove.first().click();
          await p.page.waitForTimeout(200);
          if (await chips.isVisible()) fails.push("chat: the × does not remove the image chip");
        }
      }
      // a document rides the composer as its own chip (roadmap 300): a
      // dropped text file lands in its own strip as doc1 with its file
      // name and a working ×; an image and a document dropped together
      // each land in their own strip; and Send stores the row with the
      // words intact, the document a link to its served bytes, a PDF
      // serving as a PDF
      const docs = p.page.locator("#chat-docs");
      const drop = (files) => p.page.evaluate((files) => new Promise((done) => {
        const made = [], next = (i) => {
          if (i === files.length) {
            const dt = new DataTransfer();
            made.forEach((f) => dt.items.add(f));
            const form = document.getElementById("chat-form");
            form.dispatchEvent(new DragEvent("dragover", { dataTransfer: dt, bubbles: true, cancelable: true }));
            form.dispatchEvent(new DragEvent("drop", { dataTransfer: dt, bubbles: true, cancelable: true }));
            done();
            return;
          }
          const [name, type, text] = files[i];
          if (type === "image/png") {
            const c = document.createElement("canvas");
            c.width = 320; c.height = 200;
            const g = c.getContext("2d");
            g.fillStyle = "#8a9aa8"; g.fillRect(0, 0, 320, 200);
            c.toBlob((b) => { made.push(new File([b], name, { type })); next(i + 1); }, type);
          } else {
            made.push(new File([text], name, { type }));
            next(i + 1);
          }
        };
        next(0);
      }), files);
      await drop([["spec.md", "text/markdown", "# Deploy run\n- ships 1.2.3\n"]]);
      await p.page.waitForTimeout(200);
      const docText = () => docs.innerText();
      if (!(await docs.isVisible()) || !(await docText()).includes("doc1") || !(await docText()).includes("spec.md")) fails.push("chat: a dropped text file shows no doc1 spec.md chip");
      else {
        const undoc = docs.locator("[data-undoc]");
        if (!(await undoc.count())) fails.push("chat: the document chip has no ×");
        else {
          await undoc.first().click();
          await p.page.waitForTimeout(200);
          if (await docs.isVisible()) fails.push("chat: the × does not remove the document chip");
        }
      }
      // one gesture, an image to its strip and a document to its own
      await drop([["shot.png", "image/png", ""], ["spec.md", "text/markdown", "# Deploy run\n- ships 1.2.3\n"]]);
      await p.page.waitForTimeout(200);
      if (!(await chips.isVisible()) || !(await chips.innerText()).includes("img1")) fails.push("chat: the mixed drop shows no img1");
      if (!(await docs.isVisible()) || !(await docs.innerText()).includes("doc1")) fails.push("chat: the mixed drop shows no doc1");
      // the send stores the row: the words intact beside the riding
      // files. The just-sent row is optimistic (its chips object URLs)
      // until a read carries the stored ids; the lab's turn always fails
      // (the operator door is closed), and the read that patches the row
      // in place must come even then, or the chips die with their object
      // URLs. A reload reads the thread back, and there the document is
      // a link chip to its served bytes and the PDF serves as a PDF
      const storedHref = (file) => p.page.waitForFunction((file) => {
        const a = Array.from(document.querySelectorAll("#chat-log .chat-chips .chip"))
          .find((c) => c.textContent.includes(file) && /\/chat\/attachments\/\d+$/.test(c.getAttribute("href") || ""));
        return a ? a.getAttribute("href") : false;
      }, file, { timeout: 15000 }).then((h) => h.jsonValue());
      await p.page.locator("#chat-input").click();
      await p.page.keyboard.type("here are the run notes", { delay: 20 });
      await p.page.locator("#chat-form button[type=submit]").click();
      await p.page.waitForTimeout(1200);
      const sentLog = await p.page.locator("#chat-log").innerText();
      if (!sentLog.includes("here are the run notes")) fails.push("chat: the sent message's words do not store: " + sentLog.slice(-120));
      let liveHref = null;
      try { liveHref = await storedHref("spec.md"); } catch { /* the wait's own timeout is the failure below */ }
      if (!liveHref) fails.push("chat: the failed turn leaves spec.md on its object URL, no read patches the row");
      // a PDF rides the same way
      await drop([["report.pdf", "application/pdf", "%PDF-1.7\nxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"]]);
      await p.page.waitForTimeout(200);
      await p.page.locator("#chat-input").click();
      await p.page.keyboard.type("and the report", { delay: 20 });
      await p.page.locator("#chat-form button[type=submit]").click();
      await p.page.waitForTimeout(1200);
      await p.page.reload({ waitUntil: "load" });
      await p.page.waitForTimeout(600);
      let docHref = null;
      try { docHref = await storedHref("spec.md"); } catch { /* the wait's own timeout is the failure below */ }
      if (!docHref) fails.push("chat: the stored row never links spec.md to its serving route");
      else {
        const got = await p.page.request.get(base + docHref);
        if (got.status() !== 200 || !(got.headers()["content-type"] || "").startsWith("text/")) fails.push("chat: the served document = " + got.status() + " " + got.headers()["content-type"]);
      }
      let pdfHref = null;
      try { pdfHref = await storedHref("report.pdf"); } catch { /* as above */ }
      if (!pdfHref) fails.push("chat: the stored row never links report.pdf to its serving route");
      else {
        const got = await p.page.request.get(base + pdfHref);
        if (got.status() !== 200 || (got.headers()["content-type"] || "") !== "application/pdf") fails.push("chat: the served PDF = " + got.status() + " " + got.headers()["content-type"]);
      }
      // the fifth document names the bound, the same note rule as the
      // images': a silent drop is one the member cannot know to retry
      await drop([["a.md", "text/markdown", "a\n"], ["b.md", "text/markdown", "b\n"], ["c.md", "text/markdown", "c\n"], ["d.md", "text/markdown", "d\n"], ["e.md", "text/markdown", "e\n"]]);
      await p.page.waitForTimeout(200);
      const docMany = await docs.locator("li").count();
      if (docMany !== 4) fails.push("chat: five dropped documents leave " + docMany + " chips, want 4");
      const docNotes = await p.page.locator("#chat-log .chat-error").allInnerTexts();
      if (!docNotes.some((t) => t.startsWith("Dropped 1 document") && t.includes("at most 4")))
        fails.push("chat: a dropped document names no rule (" + JSON.stringify(docNotes.slice(-2)) + ")");
      // five pasted images: four chips and a note naming the bounds. A
      // drop the composer makes silently is a drop the member cannot
      // know to retry.
      await p.page.evaluate(() => new Promise((done) => {
        const c = document.createElement("canvas");
        c.width = 8; c.height = 8;
        c.getContext("2d").fillRect(0, 0, 8, 8);
        c.toBlob((b) => {
          const dt = new DataTransfer();
          for (var i = 0; i < 5; i++) dt.items.add(new File([b], "s" + i + ".png", { type: "image/png" }));
          document.getElementById("chat-input").dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
          done();
        }, "image/png");
      }));
      await p.page.waitForTimeout(200);
      const many = await chips.locator("li").count();
      if (many !== 4) fails.push("chat: five pasted images leave " + many + " chips, want 4");
      const notes = await p.page.locator("#chat-log .chat-error").allInnerTexts();
      if (!notes.some((t) => t.startsWith("Dropped 1 image") && t.includes("at most 4") && t.includes("8 MiB")))
        fails.push("chat: a dropped image names no rule (" + JSON.stringify(notes) + ")");
      // the documents (roadmap 277): the Documents button opens the roadmap
      // in the view docked over the board, never over the chat; a link opens
      // the plan it names, Back returns, ?doc= restores, Escape closes
      const doc = p.page.locator("#doc-view");
      if (!(await p.page.locator("#documents").count())) fails.push("docs: no Documents button");
      else {
        await p.page.locator("#documents").click();
        await p.page.waitForTimeout(600);
        if (!(await doc.isVisible())) fails.push("docs: the Documents button opens no view");
        else {
          if ((await p.page.locator("#documents").getAttribute("aria-expanded")) !== "true") fails.push("docs: the button does not say it opens");
          const dt = await doc.innerText();
          for (const w of ["The roadmap", "The rounds", "build plan", "documents plan"]) if (!dt.includes(w)) fails.push('docs: missing "' + w + '"');
          // the head (item 2): the prototype's labels, the path line under
          // the head with the path, the short commit and the time of day,
          // the pins row with the roadmap marked and a + pin for the rest
          const head = await doc.locator(".doc-head").innerText();
          for (const w of ["Recent ▾", "Open in Forgejo ↗", "Read", "Edit"]) if (!head.includes(w)) fails.push('docs: the head lacks "' + w + '"');
          const pl = (await doc.locator("#doc-pathline").innerText()).replace(/\s+/g, " ");
          if (!/^docs\/roadmap\.md \S{0,7} · \d\d:\d\d/.test(pl)) fails.push('docs: the path line reads "' + pl + '"');
          const pinsRow = doc.locator("#doc-pins");
          if (!(await pinsRow.innerText()).includes("pins")) fails.push("docs: the pins row carries no label");
          const pin = pinsRow.locator('.doc-pin[data-doc="docs/roadmap.md"]');
          if (!(await pin.count()) || !(await pin.innerText()).includes("roadmap")) fails.push("docs: the roadmap is not pinned");
          else if ((await pin.getAttribute("aria-current")) !== "page") fails.push("docs: the open document's pin is not the marked tab");
          const box = await doc.boundingBox(), chat = await p.page.locator("#chat").boundingBox();
          if (box && box.x + box.width < 1430) fails.push("docs: not docked at the right edge (ends at " + Math.round(box.x + box.width) + ")");
          if (box && chat && box.x < chat.x + chat.width - 1) fails.push("docs: over the chat (from " + Math.round(box.x) + ", the chat ends at " + Math.round(chat.x + chat.width) + ")");
          await doc.locator('a[data-doc="docs/plans/2026-09-24-build-plan.md"]').click();
          await p.page.waitForTimeout(600);
          if (!(await doc.innerText()).includes("Four cards")) fails.push("docs: a link opens no plan");
          const add = doc.locator(".doc-pin-add");
          if (!(await add.count())) fails.push("docs: an unpinned document offers no + pin");
          else {
            await add.click();
            await p.page.waitForTimeout(200);
            if (!(await pinsRow.locator('.doc-pin[data-doc="docs/plans/2026-09-24-build-plan.md"]').count())) fails.push("docs: + pin does not pin the open document");
          }
          if (decodeURIComponent(p.page.url()).indexOf("doc=docs/plans/") < 0) fails.push("docs: the URL carries no doc param (" + p.page.url() + ")");
          await p.page.locator("#doc-back").click();
          await p.page.waitForTimeout(600);
          if (!(await doc.innerText()).includes("The roadmap")) fails.push("docs: Back does not return to the roadmap");
          await p.page.keyboard.press("Escape");
          await p.page.waitForTimeout(300);
          if (await doc.isVisible()) fails.push("docs: Escape does not close it");
        }
      }
      // ?doc= opens the document on load
      await p.page.goto(base + "/?doc=docs/plans/2026-10-15-documents-in-the-screen.md", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      if (!(await doc.isVisible()) || !(await doc.innerText()).includes("The documents in the screen"))
        fails.push("docs: ?doc= does not open the document on load");
      // the editor and the draft (the 2026-10-15 plan, item 3): Edit opens
      // the mono editor with its row; a keystroke is a draft — the dot on
      // the path line, the pin, the Documents button; Ctrl+S saves and the
      // dot goes; a reload restores the draft with its note and Discard
      // asks; three Ctrl+S in a slow save make two commits (item 7)
      await p.page.goto(base + "/?doc=docs/roadmap.md", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      await p.page.locator("#doc-mode-edit").click();
      await p.page.waitForTimeout(600);
      const ed = p.page.locator("#doc-editor");
      if (!(await ed.isVisible())) fails.push("editor: Edit opens no editor");
      else {
        const row = (await p.page.locator("#doc-edit-row").innerText()).replace(/\s+/g, " ");
        for (const w of ["Save as commit", "Cancel", "based on c0ffee"]) if (!row.includes(w)) fails.push('editor: the row lacks "' + w + '" (' + row + ")");
        await p.page.keyboard.type("The editor check wrote this. ");
        await p.page.waitForTimeout(300);
        if (!(await p.page.locator("#doc-dot").isVisible())) fails.push("editor: a keystroke shows no dot");
        if (!(await p.page.locator('.doc-pin[data-doc="docs/roadmap.md"]').innerText()).includes("●")) fails.push("editor: the pin carries no dot");
        if (!(await p.page.locator("#documents").innerText()).includes("●")) fails.push("editor: the Documents button carries no dot");
        await p.page.keyboard.press("Control+S");
        await p.page.waitForFunction(() => document.querySelector("#doc-facts") && /Saved: \S{7} on master/.test(document.querySelector("#doc-facts").textContent), null, { timeout: 5000 });
        if (await p.page.locator("#doc-dot").isVisible()) fails.push("editor: the dot stays after a landed save");
        // a landed save with nothing typed since returns to Read, the saved
        // version rendered, the path line keeping its Saved line
        if (await p.page.locator("#doc-edit").isVisible()) fails.push("editor: a landed save stays in the editor");
        await p.page.waitForFunction(() => (document.getElementById("doc-article").innerText || "").includes("The editor check wrote this."), null, { timeout: 5000 })
          .catch(() => fails.push("editor: the landed save does not render"));
        // the control marks Read again the moment the save lands
        if ((await p.page.locator("#doc-mode-read").getAttribute("aria-pressed")) !== "true" ||
          (await p.page.locator("#doc-mode-edit").getAttribute("aria-pressed")) !== "false")
          fails.push("editor: a landed save leaves the mode control on Edit");
        // a draft outlives the reload: the note says restored, Discard asks
        await p.page.locator("#doc-mode-edit").click();
        await p.page.waitForTimeout(300);
        await p.page.keyboard.type("A draft. ");
        await p.page.waitForTimeout(300);
        await p.page.reload({ waitUntil: "load" });
        await p.page.waitForTimeout(900);
        const note = p.page.locator("#doc-note");
        const noteText = await note.innerText().catch(() => "");
        if (!(await note.isVisible()) || !noteText.includes("draft from") || !noteText.includes("restored · discard"))
          fails.push('editor: a reload restores no draft note ("' + noteText + '")');
        else {
          if (!((await ed.inputValue()).includes("A draft."))) fails.push("editor: the restored draft lost its text");
          await p.page.locator('#doc-note [data-act="discard"]').click();
          await p.page.waitForTimeout(300);
          const ask = p.page.locator("#doc-view .doc-confirm").first();
          if (!(await ask.count()) || !(await ask.innerText()).includes("Discard your changes?")) fails.push("editor: Discard asks no confirm");
          else {
            if (!(await ask.getByRole("button", { name: "Keep" }).count())) fails.push("editor: the ask has no Keep");
            await p.page.locator('[data-act="discard-yes"]').click();
            await p.page.waitForTimeout(600);
            if (await p.page.locator("#doc-edit").isVisible()) fails.push("editor: Discard does not return to the read view");
            if (await p.page.locator("#doc-dot").isVisible()) fails.push("editor: the dot stays after Discard");
          }
        }
        // a save that refuses (the roadmap's structure): only the reason
        // keeps the refusal colour, retry is its own button the keyboard
        // reaches, and it sends the save again; its 422s are the expected
        // answers, not errors
        const refusedFrom = p.errors.length;
        await p.page.locator("#doc-mode-edit").click();
        await p.page.waitForFunction(() => { const e = document.getElementById("doc-editor"); return e && !e.value.startsWith("Reading "); }, null, { timeout: 5000 });
        await p.page.keyboard.press("Control+A");
        await p.page.keyboard.type("The refusal check wrote this.");
        await p.page.keyboard.press("Control+S");
        await p.page.waitForFunction(() => document.querySelector("#doc-facts .doc-refused"), null, { timeout: 5000 });
        const refText = (await p.page.locator("#doc-facts .doc-refused").innerText()).trim();
        if (!refText.startsWith("not saved:")) fails.push('editor: the refusal does not read not saved: ("' + refText + '")');
        if (refText.includes("retry")) fails.push('editor: the refusal reason carries the retry too ("' + refText + '")');
        const retry = p.page.locator('#doc-facts [data-act="retry"]');
        if ((await retry.count()) !== 1 || (await retry.evaluate((e) => e.tagName)) !== "BUTTON")
          fails.push("editor: the retry is not its own button");
        else {
          await retry.focus();
          if (!(await p.page.evaluate(() => { const e = document.activeElement; return !!e && e.dataset && e.dataset.act === "retry"; })))
            fails.push("editor: the keyboard cannot reach the retry");
          await p.page.keyboard.press("Enter");
          await p.page.waitForTimeout(700);
          if (!(await p.page.locator("#doc-facts .doc-refused").count())) fails.push("editor: the retry does not send the save again");
        }
        await p.page.locator('#doc-view [data-act="discard"]').first().click();
        await p.page.waitForTimeout(300);
        await p.page.locator('[data-act="discard-yes"]').click();
        await p.page.waitForTimeout(600);
        if (await p.page.locator("#doc-dot").isVisible()) fails.push("editor: the refused draft's dot stays after Discard");
        const refusals = p.errors.splice(refusedFrom);
        if (!refusals.some((e) => e.endsWith("/docs/docs/roadmap.md 422"))) fails.push("editor: the refused save answered no 422");
        p.errors.push(...refusals.filter((e) => !e.endsWith("/docs/docs/roadmap.md 422")));
        // the slow save (item 7): three Ctrl+S with typing between them
        // leave exactly two commits, and the path line says saving…
        const count = await p.page.evaluate(async (u) => (await (await fetch(u)).json()).filter((e) => e.kind === "doc_written").length, base + "/projects/ai-software-factory-code/events");
        await p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/docs-slow");
        await p.page.locator("#doc-mode-edit").click();
        await p.page.waitForTimeout(400);
        await p.page.keyboard.type("Slow. ");
        await p.page.keyboard.press("Control+S");
        await p.page.waitForTimeout(400);
        const saving = (await p.page.locator("#doc-facts").innerText()).trim();
        if (saving !== "· saving…") fails.push('editor: the slow save does not say "· saving…" ("' + saving + '")');
        await p.page.keyboard.type("More. ");
        await p.page.keyboard.press("Control+S");
        await p.page.keyboard.press("Control+S");
        await p.page.waitForFunction(() => document.querySelector("#doc-dot") && document.querySelector("#doc-dot").hidden, null, { timeout: 8000 });
        await p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/docs-slow?on=0");
        const after = await p.page.evaluate(async (u) => (await (await fetch(u)).json()).filter((e) => e.kind === "doc_written").length, base + "/projects/ai-software-factory-code/events");
        if (after !== count + 2) fails.push("editor: three Ctrl+S in a slow save left " + (after - count) + " commits, want 2");
        // the outside change over the stream (items 5+6): the read view's
        // note names who and when with a reload that reads master's new
        // text; the editor's note says the save will merge and offers
        // theirs — over other lines it lands naming whose change it
        // merged, over the same lines it refuses with the reason alone in
        // the refusal colour, theirs shown beside the editor, and
        // load-theirs asking before master's text loads
        const outside = (q) => p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/docs-change" + (q || ""));
        const noteReads = async (want, why) => {
          const t = ((await note.innerText().catch(() => "")).replace(/\s+/g, " ")).trim();
          if (!new RegExp(want).test(t)) fails.push("outside: " + why + ' ("' + t + '")');
          return !!new RegExp(want).test(t);
        };
        await outside("?line=3&text=" + encodeURIComponent("The factory builds itself in rounds; the read check's outside change landed."));
        await p.page.waitForFunction(() => { const n = document.getElementById("doc-note"); return n && !n.hidden && /changed by Sebastian \d\d:\d\d/.test(n.innerText); }, null, { timeout: 5000 })
          .catch(() => fails.push("outside: the read view notes no change"));
        if (await noteReads("^changed by Sebastian \\d\\d:\\d\\d · reload$", "the read note carries no reload"))
          await p.page.locator('#doc-note [data-act="reload"]').click();
        await p.page.waitForFunction(() => (document.getElementById("doc-article").innerText || "").includes("the read check's outside change"), null, { timeout: 5000 })
          .catch(() => fails.push("outside: the reload reads no new text"));
        // other lines: the note says the save will merge, and it does
        await p.page.locator("#doc-mode-edit").click();
        await p.page.waitForFunction(() => { const e = document.getElementById("doc-editor"); return e && !e.value.startsWith("Reading "); }, null, { timeout: 5000 });
        await p.page.keyboard.type("The merge check wrote this. ");
        await outside("?line=3");
        await p.page.waitForFunction(() => { const n = document.getElementById("doc-note"); return n && !n.hidden && /your save will merge/.test(n.innerText); }, null, { timeout: 5000 })
          .catch(() => fails.push("outside: the editor notes no change to merge"));
        await noteReads("^changed by Sebastian \\d\\d:\\d\\d · your save will merge · load theirs$", "the editor's note does not say the save will merge");
        await p.page.keyboard.press("Control+S");
        await p.page.waitForFunction(() => { const f = document.querySelector("#doc-facts"); return f && /Saved: \S{7} on master · \d\d:\d\d · merged with Sebastian's change/.test(f.textContent); }, null, { timeout: 8000 })
          .catch(() => fails.push("outside: the save over other lines lands no merged commit"));
        await p.page.waitForFunction(() => (document.getElementById("doc-article").innerText || "").includes("The merge check wrote this."), null, { timeout: 5000 })
          .catch(() => fails.push("outside: the merged save renders no text"));
        // the same lines: the save refuses, theirs shows master's text
        // with the clash marked, and load-theirs asks before it loads
        const clashFrom = p.errors.length;
        await p.page.locator("#doc-mode-edit").click();
        await p.page.waitForFunction(() => { const e = document.getElementById("doc-editor"); return e && !e.value.startsWith("Reading "); }, null, { timeout: 5000 });
        await p.page.keyboard.type("The clash check wrote this. ");
        await outside();
        await p.page.waitForFunction(() => { const n = document.getElementById("doc-note"); return n && !n.hidden && /your save will merge/.test(n.innerText); }, null, { timeout: 5000 })
          .catch(() => fails.push("outside: the editor notes no change on its own lines"));
        await p.page.keyboard.press("Control+S");
        await p.page.waitForFunction(() => document.querySelector("#doc-facts .doc-refused"), null, { timeout: 5000 })
          .catch(() => fails.push("outside: the save over the same lines refuses nothing"));
        const clash = (await p.page.locator("#doc-facts .doc-refused").innerText().catch(() => "")).trim();
        if (clash !== "not saved: Sebastian changed the same lines (14:08)") fails.push('outside: the refusal reads "' + clash + '"');
        const clashRow = (await p.page.locator("#doc-facts").innerText()).replace(/\s+/g, " ");
        for (const w of ["show theirs", "load theirs"]) if (!clashRow.includes(w)) fails.push('outside: the refused row lacks "' + w + '" (' + clashRow + ")");
        // a clash met the door's judgement: no retry, it would refuse again
        if (clashRow.includes("retry")) fails.push('outside: the refused clash carries a retry (' + clashRow + ")");
        const theirs = p.page.locator("#doc-theirs-slot");
        await p.page.locator('#doc-facts [data-act="theirs"]').click();
        await p.page.waitForTimeout(300);
        if (!(await theirs.isVisible())) fails.push("outside: show theirs opens no view");
        else {
          const tt = await theirs.innerText();
          if (!tt.includes("Changed outside the console.") || !tt.includes("read-only")) fails.push('outside: theirs is not master\'s text read-only ("' + tt.replace(/\s+/g, " ").slice(0, 80) + '…")');
          if (!(await theirs.locator(".doc-clash").count())) fails.push("outside: theirs marks no clashing line");
        }
        await p.page.locator('#doc-facts [data-act="load-theirs"]').click();
        await p.page.waitForTimeout(300);
        const loadAsk = p.page.locator("#doc-view .doc-confirm").first();
        // the question's own element, not the group: the buttons are not part of the ask
        const askText = (await loadAsk.count()) ? (await loadAsk.locator(".doc-confirm-q").innerText()).replace(/\s+/g, " ") : "";
        if (!/^Discard your changes and load the version of \d\d:\d\d\?$/.test(askText)) fails.push("outside: load theirs asks \"" + askText + "\"");
        else {
          if (!(await loadAsk.getByRole("button", { name: "Load", exact: true }).count())) fails.push("outside: the ask has no Load");
          if (!(await loadAsk.getByRole("button", { name: "Keep mine", exact: true }).count())) fails.push("outside: the ask has no Keep mine");
          await p.page.locator('[data-act="load-theirs-yes"]').click();
          await p.page.waitForFunction(() => { const e = document.getElementById("doc-editor"); return e && e.value.includes("Changed outside the console."); }, null, { timeout: 5000 })
            .catch(() => fails.push("outside: load theirs loads no master text"));
          if (await p.page.locator("#doc-dot").isVisible()) fails.push("outside: the dot stays after load theirs");
        }
        const clashes = p.errors.splice(clashFrom);
        if (!clashes.some((e) => e.endsWith("/docs/docs/roadmap.md 409"))) fails.push("outside: the refused save answered no 409");
        p.errors.push(...clashes.filter((e) => !e.endsWith("/docs/docs/roadmap.md 409")));
      }
    }
    if (sc === "busy") {
      // the find (C13): ?q= narrows every column, the counts read "n of m", the × clears
      await p.page.goto(base + "/?q=is%3Afailed", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const counts = await p.page.locator("#board .count").allInnerTexts();
      if (!counts.some((c) => /\d+ of \d+/.test(c))) fails.push("find: no count reads n of m under ?q=is:failed (" + counts.join(" | ") + ")");
      const faces = await p.page.locator("#board [data-card]").count(), failed = await p.page.locator("#board [data-card]").filter({ hasText: "failed" }).count();
      if (!faces || faces !== failed) fails.push("find: ?q=is:failed shows " + faces + " cards, " + failed + " of them failed");
      if (!(await p.page.locator("#find .find-x").isVisible())) fails.push("find: no × while a find is on");
      if ((await p.page.locator("#board").getAttribute("data-query") || "").indexOf("q=is%3Afailed") < 0) fails.push("find: the board's query does not carry q");
      // the find as you type (2026-10-15), with the board's other fields in the URL, and so
      // in a browser without moveBefore (Safari), where htmx's hx-preserve takes the find's
      // form out of the page and puts it back
      await findAsYouType(p, "?queued=oldest&routines=within&within=2&unit=hours", "export", "find as you type", fails);
      // the agent's view change (282) on the same board: the page stands under the
      // order's and the routines' menu choices, and the event's query carries the
      // find and the order, so the merge into the URL reads plainly
      await viewChange(p, "?queued=oldest&routines=within&within=2&unit=hours", fails);
      const nm = await open(browser, { width: 1440, height: 900 });
      await nm.ctx.addInitScript(() => { delete Element.prototype.moveBefore; });
      await nm.page.goto(base + "/", { waitUntil: "load" });
      if (await nm.page.evaluate(() => "moveBefore" in Element.prototype)) fails.push("find without moveBefore: the lab could not take moveBefore away");
      else await findAsYouType(nm, "", "export", "find without moveBefore", fails);
      if (nm.errors.length) fails.push("find without moveBefore: " + nm.errors.join("; "));
      await nm.ctx.close();
      // a find keeps the open drawer's hash in the URL it writes, so the drawer's ✕ still closes it;
      // the drawer at its Overview, as its Chat, read again when the stream moves, writes its hash anew
      await p.page.goto(base + "/#card=" + drawer + ":overview", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      await p.page.locator("#q").focus();
      await p.page.keyboard.type("export", { delay: 40 });
      await p.page.waitForTimeout(1200);
      const found = new URL(p.page.url());
      if (found.hash !== "#card=" + drawer || found.searchParams.get("q") !== "export") fails.push("find with a drawer: the find writes " + p.page.url() + ", want q=export and #card=" + drawer);
      await p.page.locator("#drawer-close").click();
      await p.page.waitForTimeout(400);
      const closed = new URL(p.page.url());
      if (await p.page.locator("#drawer").isVisible()) fails.push("find with a drawer: the ✕ does not close the drawer after a find");
      if (closed.hash || closed.searchParams.get("q") !== "export") fails.push("find with a drawer: after the ✕ the URL is " + p.page.url() + ", want q=export and no hash");
      // Back across a find: a drawer opened and closed (an entry of its own), a find, Back: the URL,
      // the box and the board agree, in place
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const wholeBoard = await p.page.locator("#board [data-card]").count();
      await p.page.evaluate(() => { window.labPage = true; });
      await p.page.locator('#board [data-card="' + drawer + '"]').first().click({ position: { x: 4, y: 4 } });
      await p.page.waitForTimeout(600);
      await p.page.locator("#drawer-close").click();
      await p.page.waitForTimeout(300);
      await p.page.locator("#q").click();
      await p.page.keyboard.type("export", { delay: 40 });
      await p.page.waitForTimeout(1200);
      await p.page.goBack();
      await p.page.waitForTimeout(1200);
      const agree = await p.page.evaluate(() => ({ q: new URLSearchParams(location.search).get("q") || "", box: document.getElementById("q").value,
        cards: document.querySelectorAll("#board [data-card]").length, same: window.labPage === true }));
      if (agree.q !== "" || agree.box !== "" || agree.cards !== wholeBoard || !agree.same) {
        fails.push("Back across a find: the URL's q " + JSON.stringify(agree.q) + ", the box " + JSON.stringify(agree.box) + ", " + agree.cards + " of " + wholeBoard +
          " cards, the page " + (agree.same ? "kept" : "loaded again"));
      }
      // a find, then a drawer, then Back: the drawer closes in place and the find stays
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      await p.page.evaluate(() => { window.labPage = true; });
      await p.page.locator("#q").click();
      await p.page.keyboard.type("export", { delay: 40 });
      await p.page.waitForTimeout(1200);
      const narrowed = await p.page.locator("#board [data-card]").count();
      await p.page.locator('#board [data-card="' + labID(616) + '"]').click({ position: { x: 4, y: 4 } });
      await p.page.waitForTimeout(600);
      if (!(await p.page.locator("#drawer").isVisible())) fails.push("find then drawer: a click on a found card opens no drawer");
      await p.page.goBack();
      await p.page.waitForTimeout(800);
      if (await p.page.locator("#drawer").isVisible()) fails.push("find then drawer: Back does not close the drawer");
      const stay = await p.page.evaluate(() => ({ q: new URLSearchParams(location.search).get("q"), hash: location.hash, box: document.getElementById("q").value,
        cards: document.querySelectorAll("#board [data-card]").length, same: window.labPage === true }));
      if (stay.q !== "export" || stay.hash || stay.box !== "export" || stay.cards !== narrowed || !stay.same) {
        fails.push("find then drawer: after Back the URL's q is " + stay.q + ", the hash " + JSON.stringify(stay.hash) + ", the box " + JSON.stringify(stay.box) + ", " +
          stay.cards + " of " + narrowed + " cards, the page " + (stay.same ? "kept" : "loaded again"));
      }
      // a stream swap that lands while a find is in flight leaves the find's answer on the board
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const slow = (u) => u.pathname === "/" && u.searchParams.get("q") === "export";
      await p.page.route(slow, async (r) => { await new Promise((ok) => setTimeout(ok, 1000)); await r.continue(); });
      await p.page.locator("#q").click();
      await p.page.keyboard.type("export", { delay: 40 });
      await p.page.waitForTimeout(500);
      await p.page.evaluate(() => htmx.ajax("GET", "/board" + document.getElementById("board").dataset.query, { source: "#board", target: "#board", swap: "outerHTML" }));
      await p.page.waitForTimeout(2000);
      await p.page.unroute(slow);
      const raced = await p.page.evaluate(() => ({ query: document.getElementById("board").dataset.query,
        off: Array.from(document.querySelectorAll("#board [data-card]")).filter((k) => !k.innerText.toLowerCase().includes("export")).length }));
      if (raced.query.indexOf("q=export") < 0 || raced.off) fails.push("find in a race: after a stream swap landed first the board's query is " + raced.query + ", " + raced.off + " cards do not say export");
      // a read of the board that answers after the find's answer, so in a browser without moveBefore too
      await lateRead(p.page, "late read", fails);
      const lm = await open(browser, { width: 1440, height: 900 });
      await lm.ctx.addInitScript(() => { delete Element.prototype.moveBefore; });
      await lateRead(lm.page, "late read without moveBefore", fails);
      if (lm.errors.length) fails.push("late read without moveBefore: " + lm.errors.join("; "));
      await lm.ctx.close();
      // a drawer form aimed at the board whose refusal answers after a board swap still flashes it
      await p.page.goto(base + "/#card=" + drawer + ":overview", { waitUntil: "load" });
      await p.page.waitForTimeout(800);
      const summary = p.page.locator("#drawer form.summary-edit");
      if (!(await summary.count())) fails.push("drawer refusal: the drawer has no summary form");
      else {
        const refuse = (u) => u.pathname.endsWith("/summary");
        await p.page.route(refuse, async (r) => {
          await new Promise((ok) => setTimeout(ok, 1000));
          await r.fulfill({ status: 422, contentType: "text/plain", body: "Refused: not yours." });
        });
        await summary.evaluate((f) => f.requestSubmit());
        await p.page.waitForTimeout(200);
        await p.page.evaluate(() => htmx.ajax("GET", "/board" + document.getElementById("board").dataset.query, { source: "#board", target: "#board", swap: "outerHTML" }));
        await p.page.waitForTimeout(1500);
        await p.page.unroute(refuse);
        // the refusal is the step's own, not the console's
        const own = p.errors.findIndex((x) => x.endsWith("/summary 422"));
        if (own >= 0) p.errors.splice(own, 1);
        const flash = p.page.locator("#flash"), said = (await flash.innerText()).trim();
        if (!(await flash.isVisible()) || said !== "Refused: not yours.") fails.push('drawer refusal: after a board swap the flash reads "' + said + '", shown ' + (await flash.isVisible()));
      }
      // the column menus (C13): ?done-only=failed narrows Done, its head says so with a way back
      await p.page.goto(base + "/?done-only=failed", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const state = await p.page.locator("#board .col-state").allInnerTexts();
      if (!state.some((s) => s.includes("failed only"))) fails.push("menus: no state line reads failed only under ?done-only=failed (" + state.join(" | ") + ")");
      if (!(await p.page.locator("#board .col-state .col-x").count())) fails.push("menus: no × on the state line");
      if (!(await p.page.locator('#board input[name="done-only"][value="failed"]:checked').count())) fails.push("menus: the Done menu does not show failed only as chosen");
      if ((await p.page.locator("#board .column > .col-menu").count()) !== 3) fails.push("menus: not three column menus");
      // a choice reads the board in place (Sebastian, 2026-10-14): the page does not load
      // anew, the query it stands on is one history entry, the menu stays open with the
      // focus on the choice, a second choice applies without reopening it, Escape closes it
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      await p.page.evaluate(() => { window.labStay = true; });
      const depth = await p.page.evaluate(() => history.length);
      const doneMenu = p.page.locator("#board details.col-menu:has(input[type=radio][name=done])");
      const menuNow = () => p.page.evaluate(() => {
        const a = document.activeElement;
        return { stay: window.labStay === true, depth: history.length, open: !!document.querySelector("#board details.col-menu[open] input[type=radio][name=done]"),
          focus: a && a.name ? a.name + "=" + a.value : "", state: Array.from(document.querySelectorAll("#board .col-state span")).map((x) => x.textContent).join(" | ") };
      });
      for (const [what, radio, want, state, entries] of [
        ["failed only", 'input[name="done-only"][value="failed"]', { "done-only": "failed" }, "failed only", 1],
        ["oldest first", 'input[name="done"][value="oldest"]', { "done-only": "failed", done: "oldest" }, "oldest first · failed only", 2],
      ]) {
        if (!(await doneMenu.evaluate((d) => d.open))) await doneMenu.locator("summary").click();
        await doneMenu.locator(radio).check();
        try {
          await p.page.waitForURL((u) => Object.entries(want).every(([k, v]) => u.searchParams.get(k) === v), { timeout: 5000 });
        } catch (e) {
          fails.push("menus: " + what + " does not apply (" + p.page.url() + ")");
          continue;
        }
        await p.page.waitForTimeout(400);
        const m = await menuNow(), radioFocus = radio.replace(/input\[name="([^"]+)"\]\[value="([^"]+)"\]/, "$1=$2");
        if (!m.stay) fails.push("menus: " + what + " loads the page anew");
        if (m.depth !== depth + entries) fails.push("menus: after " + what + " the history holds " + (m.depth - depth) + " entries more, want " + entries);
        if (!m.open) fails.push("menus: the menu closes after " + what);
        if (m.focus !== radioFocus) fails.push("menus: after " + what + " the focus is on " + (m.focus || "nothing") + ", want " + radioFocus);
        if (m.state !== state) fails.push('menus: after ' + what + ' the state line reads "' + m.state + '", want "' + state + '"');
      }
      await p.page.keyboard.press("Escape");
      if ((await menuNow()).open) fails.push("menus: Escape does not close the menu after a choice");
      // without script a choice waits for Apply, which loads the board under it
      const ns = await browser.newContext({ viewport: { width: 1440, height: 900 }, javaScriptEnabled: false });
      await ns.addCookies([{ name, value, domain: new URL(base).hostname, path: "/", secure: true, httpOnly: true, sameSite: "Lax" }]);
      const nsp = await ns.newPage();
      await nsp.goto(base + "/", { waitUntil: "load" });
      const nsDone = nsp.locator("#board details.col-menu:has(input[type=radio][name=done])");
      await nsDone.locator("summary").click();
      await nsDone.locator('input[name="done-only"][value="failed"]').check();
      await nsp.waitForTimeout(300);
      if (new URL(nsp.url()).searchParams.has("done-only")) fails.push("menus: without script a choice applies before Apply");
      await nsDone.getByRole("button", { name: "Apply" }).click();
      await nsp.waitForLoadState("load");
      if (new URL(nsp.url()).searchParams.get("done-only") !== "failed") fails.push("menus: without script Apply does not apply the choice (" + nsp.url() + ")");
      if (!(await nsp.locator("#board .col-state").allInnerTexts()).some((t) => t.includes("failed only"))) fails.push("menus: without script Apply leaves no state line");
      await ns.close();
    }
    if (sc === "busy") {
      // the routines (C16): every ghost at the foot of Queued, Needs you holds only the
      // filed card (the 2026-10-17 ruling), the control in the
      // board's head, the strip's cell, the chip, the routine's drawer, Skip once
      await seed("busy");
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const feet = await p.page.evaluate(() => Array.from(document.querySelectorAll("#board .column")).map((c) => {
        const kids = Array.from(c.querySelectorAll(":scope > .cards > article.card")), ghost = (k) => k.classList.contains("ghost");
        return { title: c.querySelector("h3").firstChild.textContent.trim(), first: kids.findIndex(ghost), last: kids.map((k) => !ghost(k)).lastIndexOf(true) };
      }));
      for (const f of feet) {
        if (f.title === "Queued") {
          if (f.first < 0) fails.push("routines: no ghost in Queued");
          else if (f.first < f.last) fails.push("routines: a ghost of Queued sits above its last card");
        } else if (f.first >= 0) fails.push("routines: a ghost stands in " + f.title + ", not only Queued");
      }
      const apprRef = p.page.locator('#needs-you [data-card="' + labID(607) + '"] .ref');
      if (!(await apprRef.count()) || (await apprRef.first().innerText()).trim() !== "#607") fails.push("busy: the approval card carries no ref #607");
      const radios = await p.page.locator("#board .board-head details.col-menu input[type=radio][name=routines]").count();
      if (radios !== 3) fails.push("routines: the control has " + radios + " radios named routines, not 3");
      const period = await p.page.evaluate(() => {
        const n = document.querySelector("#board .routines-ctl input[type=number][name=within]"), u = document.querySelector("#board .routines-ctl select[name=unit]");
        return n && u ? [n.min, n.max, n.value, Array.from(u.options).map((o) => o.value).join(","), u.value].join(" ") : "";
      });
      if (period !== "1 999 1 days,hours days") fails.push('routines: the control\'s number and unit read "' + period + '", want min 1, max 999, 1, days or hours, days');
      const cell = await p.page.locator(".strip .cell.routine").allInnerTexts();
      if (!cell.includes("↻ bump in 20 min")) fails.push('routines: the strip\'s cell reads "' + cell.join(" | ") + '", want ↻ bump in 20 min');
      if (!(await p.page.locator('#board [data-card="' + labID(779) + '"] .chip.routine').count())) fails.push("routines: #779 in Done carries no routine chip");
      const d = p.page.locator("#drawer");
      await p.page.locator('#board [data-routine="ai-software-factory-code/weekly-bump"]').click();
      await p.page.waitForTimeout(600);
      if (!(await d.isVisible())) fails.push("routines: a click on the ghost does not open the drawer");
      else {
        const tabs = await d.getByRole("tab").allInnerTexts();
        if (tabs.join(",") !== "Overview,Runs,Config") fails.push("routines: the drawer's tabs are " + tabs.join(", "));
        const title = (await d.locator("h2").innerText()).trim();
        if (title !== "↻ Weekly bump") fails.push('routines: the drawer\'s title is "' + title + '"');
        await p.page.keyboard.press("Escape");
        await p.page.waitForTimeout(300);
        if (await d.isVisible()) fails.push("routines: Escape does not close the routine's drawer");
      }
      // the Runs tab links each run's card, #781 the newest
      await p.page.goto(base + "/#routine=ai-software-factory-code/weekly-bump:runs", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      if (!(await d.locator('a[href="#card=' + labID(781) + '"]').count())) fails.push("routines: the Runs tab does not link #781");
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      await p.page.locator('#board [data-ghost="ai-software-factory-code/weekly-bump"]').getByRole("button", { name: "Skip Weekly bump once" }).click();
      await p.page.waitForTimeout(800);
      const note = await p.page.locator("#board .note").allInnerTexts();
      if (!note.some((t) => t.includes("Skipped once"))) fails.push("routines: Skip once does not re-render the board with Skipped once (" + note.join(" | ") + ")");
      await p.page.goto(base + "/?routines=none", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      if (await p.page.locator("#board .card.ghost").count()) fails.push("routines: ?routines=none leaves a ghost");
      const hidden = await p.page.locator("#board .ghosts-hidden").allInnerTexts();
      if (!hidden.some((t) => t.includes("routines hidden"))) fails.push("routines: ?routines=none does not say routines hidden (" + hidden.join(" | ") + ")");
      // the period (2026-10-15): choosing within, a number or a unit applies at once; the
      // dependency update runs in 3 d 0 h 20 min, the bump and the template pass in under
      // half an hour, the audit after merges, so 1 day, 2 hours and 3 days hide the
      // dependency update and 4 days none; all and none send no period, all, the default,
      // leaves the URL, and a number changed under none chooses within
      await seed("busy");
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const ctl = p.page.locator("#board .routines-ctl");
      const ghosts = async () => (await p.page.locator("#board [data-ghost]").evaluateAll((g) => g.map((x) => x.dataset.ghost.split("/")[1]))).sort().join(",");
      const all = "audit,dependency-update,template-pass,weekly-bump", soon = "audit,template-pass,weekly-bump";
      for (const [what, act, want, shown, line] of [
        ["choosing within", () => ctl.locator("input[name=routines][value=within]").check(), { routines: "within", within: "1", unit: "days" }, soon, "1 routine hidden"],
        ["a number of 4", async () => { await ctl.locator("input[name=within]").fill("4"); await ctl.locator("input[name=within]").press("Tab"); },
          { routines: "within", within: "4", unit: "days" }, all, ""],
        ["within 2 hours", async () => { await ctl.locator("input[name=within]").fill("2"); await ctl.locator("select[name=unit]").selectOption("hours"); },
          { routines: "within", within: "2", unit: "hours" }, soon, "1 routine hidden"],
        ["choosing all", () => ctl.locator("input[name=routines][value=all]").check(), { routines: null }, all, ""],
        ["choosing none", () => ctl.locator("input[name=routines][value=none]").check(), { routines: "none" }, "", "4 routines hidden"],
        ["a number of 3 under none", async () => { await ctl.locator("input[name=within]").fill("3"); await ctl.locator("input[name=within]").press("Tab"); },
          { routines: "within", within: "3", unit: "days" }, soon, "1 routine hidden"],
      ]) {
        if (!(await ctl.evaluate((d) => d.open))) await ctl.locator("summary").click();
        await act();
        try {
          await p.page.waitForURL((u) => Object.entries(want).every(([k, v]) => u.searchParams.get(k) === v), { timeout: 5000 });
        } catch (e) {
          fails.push("routines: " + what + " does not apply (" + p.page.url() + ")");
          continue;
        }
        await p.page.waitForLoadState("load");
        await p.page.waitForTimeout(600);
        const got = await ghosts(), hid = (await p.page.locator("#board .ghosts-hidden").allInnerTexts()).join(" | ");
        if (got !== shown) fails.push("routines: " + what + " shows the ghosts " + got + ", want " + shown);
        const sent = new URL(p.page.url()).searchParams;
        if (want.routines !== "within" && (sent.has("within") || sent.has("unit"))) fails.push("routines: " + what + " sends a period (" + p.page.url() + ")");
        if (hid !== line) fails.push("routines: " + what + ' says "' + hid + '" hidden, want "' + line + '"');
      }
      const kept = await p.page.evaluate(() => {
        const c = document.querySelector("#board .routines-ctl");
        return [c.querySelector("input[name=routines]:checked").value, c.querySelector("input[name=within]").value, c.querySelector("select[name=unit]").value,
          c.querySelector("summary").getAttribute("aria-label")].join(" | ");
      });
      if (kept !== "within | 3 | days | Routines on the board: within 3 days") fails.push('routines: the control does not keep within 3 days ("' + kept + '")');
      // the send with images (roadmap 300): four chips and the words go
      // as one message, the row lands with its thumbs, and a reload
      // still shows them — the store holds the images, not the page. The
      // turn itself dies on the lab's dead model door; the row and its
      // thumbs are what the check reads.
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(800);
      await p.page.evaluate(() => new Promise((done) => {
        const c = document.createElement("canvas");
        c.width = 40; c.height = 30;
        c.getContext("2d").fillRect(0, 0, 40, 30);
        c.toBlob((b) => {
          const dt = new DataTransfer();
          for (var i = 0; i < 4; i++) dt.items.add(new File([b], "ride" + i + ".png", { type: "image/png" }));
          document.getElementById("chat-input").dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true }));
          done();
        }, "image/png");
      }));
      await p.page.waitForTimeout(200);
      await p.page.locator("#chat-input").fill("Four shots ride this message.");
      await p.page.locator("#chat-form button[type=submit]").click();
      await p.page.waitForTimeout(1500);
      const sentRow = p.page.locator("#chat-log .msg.you").filter({ hasText: "Four shots ride this message." });
      if (!(await sentRow.count())) fails.push("chat: the sent message lands no row");
      else if ((await sentRow.locator(".chat-thumbs .thumb").count()) !== 4)
        fails.push("chat: the sent row carries " + (await sentRow.locator(".chat-thumbs .thumb").count()) + " thumbs, want 4");
      await p.page.reload({ waitUntil: "load" });
      await p.page.waitForTimeout(900);
      const keptRow = p.page.locator("#chat-log .msg.you").filter({ hasText: "Four shots ride this message." });
      if (!(await keptRow.count())) fails.push("chat: a reload loses the sent message's row");
      else if ((await keptRow.locator(".chat-thumbs .thumb").count()) !== 4)
        fails.push("chat: a reload loses the message's thumbs");
    }
    if (sc === "fire") {
      // the held run (C16): the ghost and the strip's cell in amber, #781 in Needs you with the chip
      if (!(await p.page.locator("#board .card.ghost.held").count())) fails.push("fire: no held ghost");
      const cell = await p.page.locator(".strip .cell.routine").allInnerTexts();
      if (!cell.includes("↻ bump held")) fails.push('fire: the strip\'s cell reads "' + cell.join(" | ") + '", want ↻ bump held');
      const k781 = p.page.locator('#needs-you [data-card="' + labID(781) + '"]');
      if (!(await k781.locator(".chip.routine").count())) fails.push("fire: #781 in Needs you carries no routine chip");
      // #781's drawer names the run that filed it and links its routine; the menus text() opened close first
      await p.page.evaluate(() => document.querySelectorAll("details.col-menu").forEach((m) => { m.open = false; }));
      await k781.click({ position: { x: 4, y: 4 } });
      await p.page.waitForTimeout(600);
      const d = p.page.locator("#drawer");
      const prov = await d.locator(".provenance").allInnerTexts();
      if (!prov.some((t) => t.includes("filed by the factory · routine Weekly bump · run 14"))) {
        fails.push('fire: #781\'s drawer lacks "filed by the factory · routine Weekly bump · run 14" (' + prov.join(" | ") + ")");
      } else {
        await d.locator(".provenance [data-open-routine]").click();
        await p.page.waitForTimeout(600);
        if (!(await d.locator("[data-routine-drawer]").count())) fails.push("fire: the provenance's routine does not open its drawer");
      }
      await p.page.keyboard.press("Escape");
    }
    if (sc === "threads") {
      // the thread table starts folded to its marks until the member unfolds it
      const fresh = await open(browser, { width: 1440, height: 900 }, true);
      await fresh.page.goto(base + "/", { waitUntil: "load" });
      await fresh.page.waitForTimeout(600);
      if (!(await fresh.page.locator(".chat.sw-collapsed").count())) fails.push("threads: the thread table does not start folded");
      await fresh.ctx.close();
      // the rows (C14): the inbox pinned first, the others by last turn: the session store
      // (6 min), the members page (9 min), the costs (41 min), the export (26 h); the members
      // page's turn in flight its one working dot; no ticker along the foot (C12)
      const order = await p.page.evaluate(() => Array.from(document.querySelectorAll("#thread-rows .trow[data-thread]"))
        .map((r) => r.classList.contains("inbox") ? "inbox" : r.dataset.thread).join(","));
      if (order !== "inbox,4,3,2,1") fails.push("threads: the rows read " + order + ", want inbox,4,3,2,1");
      const dots = await p.page.locator("#thread-rows .tmark.working").evaluateAll((d) => d.map((x) => x.closest(".trow").dataset.thread).join(","));
      if (dots !== "3") fails.push("threads: the working dots sit on " + (dots || "no row") + ", want 3");
      if (await p.page.locator("#ticker").count()) fails.push("threads: the ticker is back (C12)");
      // the commands (C14): "/" at the start of the box opens the palette, the arrows
      // choose, Escape closes it; /compact and /new also sit as chips under the box
      const box = p.page.locator("#chat-input"), pal = p.page.locator("#chat-palette");
      await box.click();
      await p.page.keyboard.type("/");
      if (!(await pal.isVisible())) fails.push("commands: / at the start of the box opens no palette");
      else {
        const opts = await pal.getByRole("option").allInnerTexts();
        if (!opts.some((o) => o.includes("/compact")) || !opts.some((o) => o.includes("/new"))) fails.push("commands: the palette lists " + opts.join(", "));
        await p.page.keyboard.press("ArrowDown");
        if (!(await pal.locator('[aria-selected="true"]').innerText()).includes("/new")) fails.push("commands: ArrowDown does not choose /new");
        await p.page.keyboard.press("Escape");
        if (await pal.isVisible()) fails.push("commands: Escape does not close the palette");
      }
      await box.fill("");
      if ((await p.page.locator("#chat-form .cmd").count()) !== 2) fails.push("commands: not two chips under the box");
      // archive and delete (C14): Delete… warns in a dialog that Keep it closes; Archive moves
      // the thread under the archived rule, and restore brings it back
      const del = p.page.locator("#thread-head [data-delete]");
      if (!(await del.count())) fails.push("threads: no Delete… on the open thread's head");
      else {
        await del.click();
        const dlg = p.page.locator("#thread-dialog");
        if (!(await dlg.isVisible())) fails.push("threads: Delete… opens no warning");
        else {
          const warn = await dlg.innerText();
          for (const w of ["Delete “Why is the session store card waiting?”?", "Its 2 messages go for good: a hard delete, no archive.",
            "One event will name you, the thread and the count.", "the events, the audit and git"]) if (!warn.includes(w)) fails.push('threads: the delete warning lacks "' + w + '"');
          await dlg.getByRole("button", { name: "Keep it" }).click();
          if (await dlg.count()) fails.push("threads: Keep it does not close the warning");
        }
      }
      await p.page.locator("#thread-head [data-archive]").click();
      await p.page.waitForTimeout(600);
      const restore = p.page.locator("#thread-rows .tarch [data-restore]");
      if (!(await restore.count())) fails.push("threads: Archive puts no row under the archived rule");
      if ((await p.page.locator("#thread-head").innerText()).includes("Why is the session store card waiting?")) fails.push("threads: the archived thread stays open");
      if (await restore.count()) {
        // an archived thread opened: its composer is one line with restore (C14)
        await p.page.locator("#thread-rows .tarch [data-thread]").first().click();
        await p.page.waitForTimeout(600);
        if (await p.page.locator("#chat-input").isVisible()) fails.push("threads: an archived thread shows the text box");
        if (!(await p.page.locator(".arch-note").isVisible())) fails.push("threads: an archived thread's composer has no restore line");
        await restore.first().click();
        await p.page.waitForTimeout(600);
        if (await p.page.locator("#thread-rows .tarch").count()) fails.push("threads: restore leaves the thread archived");
        if (!(await p.page.locator("#chat-input").isVisible())) fails.push("threads: restore does not bring the text box back");
      }
      // the fold (C14): ‹ folds the table to a strip of its marks no wider than 40 px that keeps
      // the open row's rule, the working dot and the count pill; a mark still opens its thread
      // and the table stays folded; › unfolds it
      const tg = p.page.locator("#switcher .swtg"), foldedNow = () => p.page.locator(".chat.sw-collapsed").count();
      await tg.click();
      if (!(await foldedNow())) fails.push("fold: ‹ does not fold the threads");
      else {
        const strip = await p.page.evaluate(() => {
          const sw = document.getElementById("switcher"), vis = (sel) => Array.from(sw.querySelectorAll(sel)).filter((e) => e.offsetParent && e.getBoundingClientRect().width > 0).length;
          return { width: sw.getBoundingClientRect().width, titles: vis(".ttitle"), working: vis(".tmark.working"), pills: vis(".badge, .bsplit"), on: vis(".trow.on") };
        });
        if (strip.width > 40) fails.push("fold: the folded strip is " + Math.round(strip.width) + " px wide");
        if (strip.titles) fails.push("fold: the folded strip still shows " + strip.titles + " titles");
        if (strip.working !== 1) fails.push("fold: the folded strip shows " + strip.working + " working dots, want 1");
        if (strip.pills !== 1) fails.push("fold: the folded strip shows " + strip.pills + " count pills, want the costs thread's 1");
        if (strip.on !== 1) fails.push("fold: the folded strip keeps no open row's rule");
        await p.page.locator('#thread-rows .trow[data-thread="2"]').click();
        await p.page.waitForTimeout(600);
        if (!(await p.page.locator("#thread-head").innerText()).includes("Give the costs page a from and a to date.")) fails.push("fold: a mark does not open its thread");
        if (!(await foldedNow())) fails.push("fold: opening a thread unfolds the table");
        await tg.click();
        if (await foldedNow()) fails.push("fold: › does not unfold the threads");
      }
      // delete is hard (C14): Delete the thread takes the costs thread's row
      await p.page.locator('#thread-rows .trow[data-thread="2"]').click();
      await p.page.waitForTimeout(600);
      await p.page.locator("#thread-head [data-delete]").click();
      await p.page.locator('#thread-dialog [data-dlg="delete"]').click();
      await p.page.waitForTimeout(600);
      if (await p.page.locator("#thread-dialog").count()) fails.push("threads: Delete the thread leaves the warning open");
      if (await p.page.locator('#thread-rows .trow[data-thread="2"]').count()) fails.push("threads: Delete the thread leaves its row");
      // the commands run (C14): "/ne" and Enter in the palette start a new thread, as the
      // /new chip does, each opening empty with the focus in the box
      for (const [what, run] of [
        ["/ne and Enter", async () => { await box.click(); await p.page.keyboard.type("/ne"); await p.page.keyboard.press("Enter"); }],
        ["the /new chip", () => p.page.locator('#chat-form .cmd[data-cmd="new"]').click()],
      ]) {
        await p.page.locator('#thread-rows .trow[data-thread="4"]').click();
        await p.page.waitForTimeout(600);
        await run();
        await p.page.waitForTimeout(800);
        if (await pal.isVisible()) fails.push("commands: the palette stays open after " + what);
        const head = (await p.page.locator("#thread-head").innerText()).trim();
        if (!head.startsWith("New thread")) fails.push('commands: ' + what + ' opens "' + head.split("\n")[0] + '", not a new thread');
        if (await p.page.locator("#chat-log .msg").count()) fails.push("commands: the thread " + what + " opens is not empty");
        if (await p.page.evaluate(() => document.activeElement && document.activeElement.id) !== "chat-input") fails.push("commands: after " + what + " the focus is not in the box");
      }
    }
    if (sc === "assisted") {
      // the level control under Send (C14, C32): the four states with their words, no
      // confirm; a pick closes the menu and sets the control at once with no pending look,
      // the write lands behind it, the lamp's blue follows the level, Escape closes the menu
      const btn = p.page.locator("#level-btn"), pop = p.page.locator(".lvlpop");
      const lamp = () => btn.locator("i").evaluate((e) => getComputedStyle(e).backgroundColor);
      if ((await btn.getAttribute("data-level")) !== "assisted") fails.push("level: the control does not show assisted");
      const lampBefore = await lamp();
      await btn.click();
      if ((await pop.locator("[data-lvl]").count()) !== 4 || (await pop.locator(".lw").count()) !== 4) fails.push("level: the menu does not list the four states with their words");
      if ((await pop.locator('[aria-checked="true"]').getAttribute("data-lvl")) !== "assisted") fails.push("level: the menu does not check the current state");
      if (await pop.locator("[data-set], [data-keep]").count()) fails.push("level: the menu still carries a confirm");
      await p.page.keyboard.press("Escape");
      if (await pop.isVisible()) fails.push("level: Escape does not close the menu");
      await btn.click();
      await pop.locator('[data-lvl="full auto"]').click();
      if (await pop.isVisible()) fails.push("level: a pick leaves the menu open");
      if ((await btn.getAttribute("data-level")) !== "full auto") fails.push("level: a pick does not set the control at once");
      if (await btn.isDisabled()) fails.push("level: the control greys while the write runs");
      await p.page.waitForTimeout(700);
      if ((await btn.getAttribute("data-level")) !== "full auto") fails.push("level: the write did not land, the control reads " + (await btn.getAttribute("data-level")));
      if ((await lamp()) === lampBefore) fails.push("level: the lamp's blue does not follow the level");
      // the agent's own start carries its decider line in the inbox (C14)
      await p.page.locator("#thread-rows .trow.inbox").click();
      await p.page.waitForTimeout(600);
      const deciders = await p.page.locator("#chat-log .decider").allInnerTexts();
      if (deciders.length !== 1 || !deciders[0].includes("assisted level · started by the agent")) fails.push("assisted: the inbox's deciders read " + JSON.stringify(deciders));
      if (await p.page.locator("form#autonomy").count()) fails.push("level: the board head still carries the old selector");
    }
    if (sc === "escalation") {
      // the answer row (C14): the pending escalation's choices once, between the log and the box;
      // a choice answers its card with no model turn and the row goes.
      // A settled ask sits over the escalation: the row still offers the
      // escalation, once, not the ask's buttons and not silence.
      const row = p.page.locator("#chat-answer");
      const once = await p.page.locator("#chat").getByRole("button", { name: "Postgres", exact: true }).count();
      if (once !== 1) fails.push("escalation: the chat shows the choice Postgres " + once + " times, want once");
      if (!(await row.isVisible())) fails.push("escalation: no answer row under the inbox's escalation");
      else {
        await row.getByRole("button", { name: "Postgres" }).click();
        await p.page.waitForTimeout(800);
        if (await row.isVisible()) fails.push("escalation: the answer row stays after a choice");
        const log = await p.page.locator("#chat-log").innerText();
        if (!log.includes("Answered #603")) fails.push("escalation: the factory's row on the answer is missing");
      }
    }
    if (sc === "threads") {
      // Ctrl+k reaches the threads: the first row, the inbox (C14)
      await p.page.locator("#chat-log").click();
      await p.page.keyboard.press("Control+k");
      if (!(await p.page.evaluate(() => !!document.activeElement && !!document.activeElement.closest("#thread-rows")))) fails.push("threads: Ctrl+k does not reach the switcher");
    }
    if (sc === "poke") {
      // a poke on another thread: the inbox row first, its split badge, and the tab title's count of what waits (C21, the ruling of 2026-10-19)
      const first = p.page.locator("#thread-rows .trow").first();
      if (!(await first.evaluate((e) => e.classList.contains("inbox")))) fails.push("poke: the inbox is not the first row");
      if (!(await first.evaluate((e) => e.classList.contains("unread")))) fails.push("poke: the inbox row carries no unread badge");
      const badge = first.locator(".badge, .bsplit").first();
      if (!(await badge.count()) || (await badge.getAttribute("aria-label")) !== "1 needs you · 1 new")
        fails.push('poke: the inbox row carries no split badge "1 needs you · 1 new"');
      // the tab title counts the one card that waits for the member now, the
      // ask on #606; new rows and unseen answers never count (2026-10-19
      // ruling); the poke leaves the session store thread open (C14)
      const title = await p.page.title();
      if (!title.startsWith("(1) ")) fails.push('poke: the tab title is "' + title + '", want the one card that waits, "(1) " first');
      if ((await p.page.locator("#thread-head").innerText()).includes("uninvited")) fails.push("poke: the poke opens the inbox by itself");
      await first.click();
      await p.page.waitForTimeout(600);
      if (!(await p.page.locator("#thread-head").innerText()).includes("the factory's thread: the agent speaks here uninvited")) fails.push("poke: the inbox's head does not say it is the factory's thread");
      const log = await p.page.locator("#chat").innerText();
      for (const w of ["#600 is done", "needs you", "#606 waits for you: merge PR #612", "moved on ·"]) if (!log.includes(w)) fails.push('poke: the inbox shows no "' + w + '"');
      const settled = p.page.locator("#chat-log .msg.esc.settled");
      if (!(await settled.count())) fails.push("poke: the answered merge ask does not sit settled");
      else if (await settled.locator("button").count()) fails.push("poke: the settled ask keeps its buttons");
      if (!(await p.page.title()).startsWith("(1) ")) fails.push("poke: opening the inbox drops the waiting ask from the tab title");
      // the merge ask's numbers: the PR stays text, the card's ref is the
      // link; the link and the row's body open the card's drawer and
      // light the card. The ask sits settled here, so its fold opens the
      // row first.
      if (await p.page.locator('#chat-log a.cardref[data-issue="612"]').count()) fails.push("poke: the PR number is a card link");
      const askRow = p.page.locator('#chat-log .msg.esc[data-card="' + labID(606) + '"]').first();
      const askFold = askRow.locator("[data-fold]");
      if (await askFold.count()) await askFold.click();
      await p.page.waitForTimeout(150);
      const ref = askRow.locator('a.cardref[data-issue="606"]');
      if (!(await ref.count())) fails.push("poke: the card's ref is no link");
      await ref.first().click();
      await p.page.waitForTimeout(400);
      let hash = decodeURIComponent(await p.page.evaluate(() => location.hash));
      if (hash !== "#card=" + labID(606)) fails.push('poke: the ref link opens no drawer, hash "' + hash + '"');
      if (!(await p.page.locator('#board [data-card="' + labID(606) + '"].lit').count())) fails.push("poke: the ref link lights no card");
      await p.page.keyboard.press("Escape");
      await askRow.locator(".said").first().click();
      await p.page.waitForTimeout(300);
      hash = decodeURIComponent(await p.page.evaluate(() => location.hash));
      if (hash !== "#card=" + labID(606)) fails.push('poke: the row body opens no drawer, hash "' + hash + '"');
      await p.page.keyboard.press("Escape");
      // Clear old (the sweep): a shrunk log holds the member's scroll over
      // a background refresh, the sweep takes the settled rows from view,
      // the open ask stays with its buttons, the cleared line counts, and
      // the button greys until a new row lands
      const logEl = p.page.locator("#chat-log");
      await logEl.evaluate((e) => { e.style.height = "220px"; });
      await logEl.evaluate((e) => { e.scrollTop = 0; });
      await p.page.evaluate(() => document.dispatchEvent(new CustomEvent("factoryevent", { detail: { kind: "task_ended" } })));
      await p.page.waitForTimeout(700);
      if ((await logEl.evaluate((e) => e.scrollTop)) > 40) fails.push("poke: a background refresh yanks the member's scroll to the newest rows");
      const clearBtn = p.page.locator(".thead [data-clear]");
      if (!(await clearBtn.count())) fails.push("poke: the head carries no Clear old");
      else {
        if (await clearBtn.isDisabled()) fails.push("poke: Clear old is greyed with rows to sweep");
        const openAsk = p.page.locator("#chat-log .msg.esc:not(.settled)");
        if (!(await openAsk.count())) fails.push("poke: no open ask to keep over the sweep");
        await clearBtn.click();
        await p.page.waitForTimeout(700);
        if (!(await clearBtn.isDisabled())) fails.push("poke: Clear old stays lit with nothing left to sweep");
        if (!(await openAsk.count())) fails.push("poke: the sweep took the open ask");
        else if (!(await p.page.locator("#chat-answer").getByRole("button", { name: "Postgres" }).count()))
          fails.push("poke: the open ask lost its answer row over the sweep");
        const line = p.page.locator("#chat-log .chat-cleared");
        if (!(await line.count()) || !(await line.innerText()).includes("cleared: 4 older messages"))
          fails.push("poke: the cleared line does not count the four swept rows");
        if ((await p.page.locator("#chat-log .msg").count()) !== 1) fails.push("poke: the log kept old rows over the sweep");
      }
    }
    if (sc === "wake") {
      // one wake turn is one row (the main agent concept): it folds to the
      // wake's first line, the turn's text sits behind the fold, a click
      // opens the whole turn and the fold says which state it is in
      const row = p.page.locator("#chat-log .msg.wake");
      if ((await row.count()) !== 1) fails.push("wake: " + (await row.count()) + " wake rows, want 1");
      else {
        const fold = row.locator("[data-fold]");
        if (!(await fold.count())) fails.push("wake: the row carries no fold");
        else {
          if (!(await row.locator(".wake-b").count())) fails.push("wake: the row carries no expandable body");
          if ((await fold.innerText()).replace(/\s+/g, " ").indexOf("#603: Which store keeps the nightly board snapshots?") < 0)
            fails.push("wake: the fold does not lead with the wake's first line");
          if ((await row.innerText()).includes("Read each card")) fails.push("wake: the folded row shows the turn's text");
          if ((await row.innerText()).includes("Both stores hold the snapshots")) fails.push("wake: the folded row shows the agent's answer");
          if ((await fold.getAttribute("aria-expanded")) !== "false") fails.push("wake: the fold does not say closed");
          await fold.click();
          await p.page.waitForTimeout(150);
          const open = await row.innerText();
          for (const w of ["Read each card", "Both stores hold the snapshots", "card"]) if (!open.includes(w)) fails.push('wake: the opened row hides "' + w + '"');
          if ((await fold.getAttribute("aria-expanded")) !== "true") fails.push("wake: the opened fold does not say open");
        }
      }
    }
    if (p.errors.length) fails.push(sc + ": " + p.errors[0]);
    await p.ctx.close();
  }
  // the roll's gap: the router answers 503 to the event stream while the
  // console's pod rolls, and a refused connect closes the browser's
  // EventSource for good; the board must reopen the stream itself and read
  // its cards again, not stand frozen until a reload (a done card stayed in
  // its column while its notification arrived, 2026-10-18)
  await seed("busy");
  {
    const pp = await open(browser, { width: 1440, height: 900 });
    await pp.page.goto(base + "/", { waitUntil: "load" });
    await pp.page.waitForTimeout(600);
    const busy = await pp.page.evaluate(() => document.getElementById("status-line").innerText);
    await fetch(base + "/lab/stream-fail?n=1", { method: "POST" });
    await pp.page.reload({ waitUntil: "load" });
    await pp.page.waitForTimeout(600);
    const down = await pp.page.evaluate(() => document.getElementById("live").textContent);
    if (!/down/.test(down)) fails.push("gap: the 503 leaves the footer not saying the stream is down (" + down + ")");
    await seed("idle");
    await pp.page.waitForTimeout(9000);
    const live = await pp.page.evaluate(() => document.getElementById("live").textContent);
    if (live) fails.push("gap: the closed stream reopens by itself (" + live + ")");
    const idle = await pp.page.evaluate(() => document.getElementById("status-line").innerText);
    if (idle === busy) fails.push("gap: the reopened stream reads no board again (" + String(idle).slice(0, 60) + ")");
    await pp.ctx.close();
  }
  // a narrow desk at 960: the strip's cells wrap where they do not fit, the
  // board's head fits its row, the documents' head wraps in its panel
  for (const sc of ["busy", "fire"]) {
    await seed(sc);
    const np = await open(browser, { width: 960, height: 800 });
    await np.page.goto(base + "/", { waitUntil: "load" });
    await np.page.waitForTimeout(600);
    const nsw = await sideways(np.page);
    if (nsw) fails.push("960: horizontal scroll in " + sc + " (" + nsw + " px)");
    const head = await np.page.evaluate(() => { const h = document.querySelector(".board-head"); return h.scrollWidth - h.clientWidth; });
    if (head > 0) fails.push("960: the board's head overflows its row by " + head + " px in " + sc);
    await np.page.locator("#board .documents").click();
    await np.page.waitForTimeout(600);
    const dsw = await sideways(np.page);
    if (dsw) fails.push("960: horizontal scroll with the documents open in " + sc + " (" + dsw + " px)");
    await np.ctx.close();
  }
  // a table wider than the phone scrolls in its own box, not the page
  {
    const card = await seed("busy");
    for (const u of ["/costs", "/resources", "/policy", "/tasks/" + card + "/timeline"]) {
      const tp = await open(browser, { width: 390, height: 844 });
      await tp.page.goto(base + u, { waitUntil: "load" });
      await tp.page.waitForTimeout(400);
      const tsw = await sideways(tp.page);
      if (tsw) fails.push("phone: horizontal scroll at 390 on " + u + " (" + tsw + " px)");
      await tp.ctx.close();
    }
  }
  // the panel on the phone: the rows stack, every target reaches 44 px, no horizontal scroll
  await seed("busy");
  {
    const pp = await open(browser, { width: 390, height: 844 });
    await pp.page.goto(base + "/notifications", { waitUntil: "load" });
    await pp.page.waitForTimeout(600);
    if (!(await pp.page.locator(".notify thead").first().isHidden())) fails.push("phone: the panel's headers still show at 390");
    const stacked = await pp.page.evaluate(() => Array.from(document.querySelectorAll(".notify tbody tr")).map((tr) => getComputedStyle(tr).display));
    if (stacked.length !== 5 || stacked.includes("table-row")) fails.push("phone: the panel's rows do not stack: " + JSON.stringify(stacked));
    const small = await pp.page.evaluate(() => Array.from(document.querySelectorAll(".notify td.act a, .notify td.act button, .notify td.act select"))
      .filter((b) => b.offsetParent).map((b) => [b.textContent.trim().slice(0, 16) || b.tagName, b.getBoundingClientRect().height]).filter((x) => x[1] < 43.5));
    if (small.length) fails.push("phone: panel targets under 44 px: " + small.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
    const psw = await sideways(pp.page);
    if (psw) fails.push("phone: horizontal scroll at 390 on the panel (" + psw + " px)");
    await pp.ctx.close();
  }
  // the phone: 44 px targets for the C14 controls and the bar's, no horizontal scroll
  for (const sc of ["busy", "threads", "escalation"]) {
    await seed(sc);
    const pp = await open(browser, { width: 390, height: 844 });
    await pp.page.goto(base + "/", { waitUntil: "load" });
    await pp.page.waitForTimeout(600);
    const small = await pp.page.evaluate(() => Array.from(document.querySelectorAll(
      ".trow, .msg .choices button, .tacts button, .cmd, .restore, #level-btn, .open-arch, .swtg, .sw-sum, .answer button, " +
      "header nav a, .menu > summary, .menu .s-out button, .switch select, .theme-switch button"))
      .filter((b) => b.offsetParent).map((b) => [b.className || b.id || b.tagName, b.getBoundingClientRect().height]).filter((x) => x[1] < 43.5));
    if (small.length) fails.push("phone: targets under 44 px in " + sc + ": " + small.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
    const psw = await sideways(pp.page);
    if (psw) fails.push("phone: horizontal scroll at 390 in " + sc + " (" + psw + " px)");
    await pp.ctx.close();
  }
  // the phone's fold (C14): folded by default to one summary row, the five threads, the costs
  // thread's new answer as a pill and the one turn in flight, the list out of view; a tap
  // unfolds it
  await seed("threads");
  {
    const pf = await open(browser, { width: 390, height: 844 }, true);
    await pf.page.goto(base + "/", { waitUntil: "load" });
    await pf.page.waitForTimeout(600);
    const sumRow = pf.page.locator(".sw-sum");
    if (!(await sumRow.isVisible())) fails.push("phone: the folded threads show no summary row");
    else {
      const said = (await sumRow.innerText()).replace(/\s+/g, " "), pill = await sumRow.locator(".badge").first().getAttribute("aria-label").catch(() => "");
      if (!said.startsWith("Threads 5 ") || !said.includes("1 working") || pill !== "1 new") fails.push('phone: the summary row reads "' + said + '" with the pill "' + pill + '"');
      if (await pf.page.locator("#thread-rows").isVisible()) fails.push("phone: the folded threads leave the list in view");
      await sumRow.click();
      if (!(await pf.page.locator("#thread-rows").isVisible())) fails.push("phone: a tap on the summary row does not unfold the threads");
    }
    await pf.ctx.close();
  }
  // the documents on the phone: the view covers the screen, its controls
  // reach 44 px, Escape closes, no horizontal scroll
  await seed("busy");
  {
    const pp = await open(browser, { width: 390, height: 844 });
    await pp.page.goto(base + "/", { waitUntil: "load" });
    await pp.page.waitForTimeout(600);
    await pp.page.locator("#documents").click();
    await pp.page.waitForTimeout(600);
    if (!(await pp.page.locator("#doc-view").isVisible())) fails.push("phone: the documents do not open");
    else {
      const small = await pp.page.evaluate(() => Array.from(document.querySelectorAll(".doc-row .xs, .doc-recent > summary, .doc-mode button, .doc-pin, .doc-pin-add, #doc-forgejo"))
        .filter((b) => b.offsetParent).map((b) => [b.id || b.className, b.getBoundingClientRect().height]).filter((x) => x[1] < 43.5));
      if (small.length) fails.push("phone: doc targets under 44 px: " + small.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
      // every head control stands in view at 390: the nav row, the path
      // line's Forgejo link, Read|Edit, the pins — none cut off, no
      // horizontal scroll
      const cut = await pp.page.evaluate(() => Array.from(document.querySelectorAll(
        "#doc-view .doc-head button, #doc-view .doc-head a, #doc-view .doc-head summary, #doc-view .doc-pin, #doc-view .doc-pin-add"))
        .filter((b) => b.offsetParent).map((b) => [b.id || b.className, Math.round(b.getBoundingClientRect().left), Math.round(b.getBoundingClientRect().right)])
        .filter((x) => x[1] < 0 || x[2] > 390));
      if (cut.length) fails.push("phone: a document head control is cut off at 390: " + cut.map((x) => x[0] + " " + x[1] + ".." + x[2]).join(", "));
      // New document shows its whole text in the Read/Edit row, not a
      // clipped word in a square box
      const nb = await pp.page.evaluate(() => { const b = document.getElementById("doc-new"); return { t: b.innerText.trim(), w: b.scrollWidth - b.clientWidth, h: b.scrollHeight - b.clientHeight }; });
      if (nb.t !== "New document" || nb.w > 1 || nb.h > 1) fails.push('phone: New document does not show its whole text ("' + nb.t + '", ' + nb.w + "x" + nb.h + "px over)");
      const psw = await sideways(pp.page);
      if (psw) fails.push("phone: horizontal scroll at 390 with the documents (" + psw + " px)");
      await pp.page.keyboard.press("Escape");
      await pp.page.waitForTimeout(300);
      if (await pp.page.locator("#doc-view").isVisible()) fails.push("phone: Escape does not close the documents");
    }
    await pp.ctx.close();
  }
  // the messenger panel (roadmap 145, C19): both tables' words, a lamp on
  // every state — cyan for connected and a set level, amber for
  // needs-you, an unlit socket for off and not connected; the phone's
  // rows stack and every target reaches 44 px
  await seed("busy");
  {
    const np = await open(browser, { width: 1440, height: 900 });
    await np.page.goto(base + "/notifications", { waitUntil: "load" });
    await np.page.waitForTimeout(600);
    const words = await np.page.evaluate(() => document.body.innerText);
    for (const w of ["Connections", "telegram", "connected", "@sebastian", "Disconnect", "discord", "not connected", "Connect",
      "Level per project", "default:", "off", "needs-you", "all", "Save"]) if (!words.includes(w)) fails.push("panel: misses " + JSON.stringify(w));
    const lamps = await np.page.evaluate(() => Array.from(document.querySelectorAll(".notify .lamp")).map((l) => l.className.trim()));
    if (lamps.length !== 5) fails.push("panel: " + lamps.length + " lamps, want 5 (two venues, three projects): " + JSON.stringify(lamps));
    if (!lamps.includes("lamp on")) fails.push("panel: no lit cyan lamp (a connected venue, a set level): " + JSON.stringify(lamps));
    if (!lamps.includes("lamp needs-you")) fails.push("panel: no amber lamp (a needs-you default): " + JSON.stringify(lamps));
    if (!lamps.includes("lamp")) fails.push("panel: no unlit lamp (not connected, off): " + JSON.stringify(lamps));
    const sizes = await np.page.evaluate(() => Array.from(document.querySelectorAll(".notify .lamp")).map((l) => [l.className.trim(), l.offsetWidth, l.offsetHeight]));
    const big = sizes.filter((s) => s[1] > 11 || s[2] > 11);
    if (big.length) fails.push("panel: a lamp is not the 8px square: " + big.map((s) => s.join(" ")).join("; "));
    const tails = await np.page.evaluate(() => Array.from(document.querySelectorAll(".notify td.act")).map((td) => Math.round(td.getBoundingClientRect().right - td.querySelector("button, a").getBoundingClientRect().right)));
    if (tails.some((g) => g > 24)) fails.push("panel: the act controls leave an empty tail to the row's rule: " + tails.join(","));
    await np.ctx.close();
  }
  // the contrast: every text pair of the C14 parts, both themes, 4.5:1 (3:1 for large text)
  for (const theme of ["dark", "light"]) {
    for (const [sc, act, sel, pg] of [["threads", "delete", contrastEl, ""], ["escalation", "palette", contrastEl, ""], ["assisted", "level", contrastEl, ""],
      ["poke", "", contrastEl, ""], ["busy", "docs", contrastEl, ""], ["busy", "", ghostEl, ""], ["fire", "", ghostEl, ""], ["busy", "", notifyEl, "/notifications"],
      ["metrics", "", metricsEl, metricsPath]]) {
      await seed(sc);
      const cp = await open(browser, { width: 1440, height: 900 });
      // after open's own init script, so this theme wins
      await cp.ctx.addInitScript((t) => localStorage.setItem("theme", t), theme);
      await cp.page.goto(base + (pg || "/"), { waitUntil: "load" });
      if ((await cp.page.evaluate(() => document.documentElement.dataset.theme)) !== theme) fails.push("contrast: the page is not in the " + theme + " theme");
      await cp.page.waitForTimeout(600);
      if (act === "delete") await cp.page.locator("#thread-head [data-delete]").click();
      if (act === "palette") { await cp.page.locator("#chat-input").click(); await cp.page.keyboard.type("/"); }
      if (act === "level") await cp.page.locator("#level-btn").click();
      if (act === "docs") {
        await cp.page.locator("#documents").click();
        await cp.page.waitForTimeout(600);
        // the editor with its row and the dot of a draft
        await cp.page.locator("#doc-mode-edit").click();
        await cp.page.waitForTimeout(400);
        await cp.page.keyboard.type("a");
        await cp.page.waitForTimeout(200);
      }
      await cp.page.waitForTimeout(200);
      const low = await cp.page.evaluate((sel) => {
        const cv = document.createElement("canvas").getContext("2d", { willReadFrequently: true });
        const rgb = (s) => { cv.clearRect(0, 0, 1, 1); cv.fillStyle = s; cv.fillRect(0, 0, 1, 1); const d = cv.getImageData(0, 0, 1, 1).data; return [d[0], d[1], d[2], d[3] / 255]; };
        const over = (fg, bg) => fg.map((x, i) => (i < 3 ? x * fg[3] + bg[i] * (1 - fg[3]) : 1));
        const lum = (c) => { const f = (x) => { x /= 255; return x <= 0.03928 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4); }; return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]); };
        const bgOf = (el) => {
          const layers = [];
          for (let e = el; e; e = e.parentElement) { const b = rgb(getComputedStyle(e).backgroundColor); if (b[3] > 0) { layers.push(b); if (b[3] >= 1) break; } }
          let out = [255, 255, 255, 1];
          for (const l of layers.reverse()) out = over(l, out);
          return out;
        };
        const out = [];
        let measured = 0;
        for (const el of document.querySelectorAll(sel)) {
          if (!el.offsetParent || !el.textContent.trim()) continue;
          measured++;
          const cs = getComputedStyle(el), bg = bgOf(el), fg = over(rgb(cs.color), bg);
          const l1 = lum(fg), l2 = lum(bg), r = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
          const big = parseFloat(cs.fontSize) >= 24 || (parseFloat(cs.fontSize) >= 18.66 && +cs.fontWeight >= 700);
          if (r < (big ? 3 : 4.5)) out.push((el.className || el.tagName) + ' "' + el.textContent.trim().slice(0, 18) + '" ' + r.toFixed(2));
        }
        return { out, measured };
      }, sel);
      if (!low.measured) fails.push(theme + " " + sc + ": the contrast check matched no shown text");
      if (low.out.length) fails.push(theme + " " + sc + ": under 4.5:1: " + low.out.join("; "));
      await cp.ctx.close();
    }
  }
  await seed("busy");
  const ph = await open(browser, { width: 390, height: 844 });
  await ph.page.goto(base + "/", { waitUntil: "load" });
  await ph.page.waitForTimeout(600);
  const sw = await sideways(ph.page);
  if (sw) fails.push("phone: horizontal scroll at 390 (" + sw + " px)");
  const ghostTargets = await ph.page.evaluate(() => Array.from(document.querySelectorAll(".card.ghost > button.open, .card.ghost .acts button, .routines-ctl > summary"))
    .filter((b) => b.offsetParent).map((b) => [b.textContent.trim().slice(0, 20), b.getBoundingClientRect().height]));
  // every shown ghost opens its drawer from its title, and the control is there
  const ghostsShown = await ph.page.evaluate(() => Array.from(document.querySelectorAll(".card.ghost")).filter((g) => g.offsetParent)
    .map((g) => !!g.querySelector(":scope > button.open")));
  if (!ghostsShown.length) fails.push("phone: no ghost shown");
  if (ghostsShown.includes(false)) fails.push("phone: a shown ghost has no button to its drawer");
  if (!(await ph.page.locator(".routines-ctl > summary").first().isVisible())) fails.push("phone: the routines control is not shown");
  const smallGhost = ghostTargets.filter((x) => x[1] < 43.5);
  if (smallGhost.length) fails.push("phone: routine targets under 44 px: " + smallGhost.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
  // the routines control open: its three choices, the period's number and its unit
  await ph.page.locator(".routines-ctl > summary").first().click();
  await ph.page.waitForTimeout(200);
  const ctlTargets = await ph.page.evaluate(() => Array.from(document.querySelectorAll(".routines-ctl .menu-pop label, .routines-ctl .menu-pop input[type=number], .routines-ctl .menu-pop select"))
    .filter((b) => b.offsetParent).map((b) => [b.name || b.textContent.trim(), b.getBoundingClientRect().height, b.getBoundingClientRect().right]));
  if (ctlTargets.length !== 5) fails.push("phone: the open routines control shows " + ctlTargets.length + " targets, want 3 choices, the number and the unit");
  const smallCtl = ctlTargets.filter((x) => x[1] < 43.5);
  if (smallCtl.length) fails.push("phone: routines control targets under 44 px: " + smallCtl.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
  const cutCtl = ctlTargets.filter((x) => x[2] > 390);
  if (cutCtl.length) fails.push("phone: routines control targets cut off at 390: " + cutCtl.map((x) => x[0] + " to " + Math.round(x[2])).join(", "));
  await ph.page.locator(".routines-ctl > summary").first().click();
  const order = await ph.page.evaluate(() => {
    const c = document.querySelector("#chat"), b = document.querySelector("#board");
    const vis = (e) => e && e.getClientRects().length > 0;
    return c && b ? { chat: vis(c) && c.getBoundingClientRect().top, board: vis(b) && b.getBoundingClientRect().top } : null;
  });
  if (!order) fails.push("phone: no #chat and #board");
  else if (order.chat === false || order.board === false) fails.push("phone: the chat and the board are not both shown (chat " + order.chat + ", board " + order.board + ")");
  else if (order.chat > order.board) fails.push("phone: the board sits above the chat");
  await ph.ctx.close();
  await seed("busy");
  const cp = await open(browser, { width: 1440, height: 900 });
  await cardPress(cp.page, fails);
  await cp.ctx.close();
  await browser.close();
  if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
  console.log("ok: every control present, the mechanics work");
})().catch((e) => { console.error(e.message || e); process.exit(1); });
