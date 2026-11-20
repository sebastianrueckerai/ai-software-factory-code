// check.js is the structural check of the console design round: against
// the lab (run.sh starts it) it reads every scenario in the dark theme
// and fails on a control of design/inventory.md that is missing, or a
// mechanic that does not work: the drawer docked at the right edge and
// never over the chat, Escape closing it, Do it becoming an applied line,
// the routines' ghosts at the foot of their lanes with their control and
// drawer and their period, the find narrowing the board as you type, a
// card's press moving the card at once and taking one press, the inbox
// taking presses through /answer and no words, the chat above the board
// at 390 px, no horizontal scroll at 1440, 960 or 390,
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
  ".msg.poke .who, .thead .ttl, .thr span, .tacts button, .overlay-box p, .overlay-box h2, .answer button, .arch-note, .inbox-note, .thread-link, " +
  ".doc-path, .doc-facts, .doc-refused, .doc-dot, .doc-base, .doc-note, .doc-confirm-q, .doc-new-l, .doc-discard, #doc-new, " +
  ".doc-recent > summary, .doc-mode button, .doc-recent-label, .doc-none, #doc-article a, .doc-pins-l, .doc-pin, .doc-pin-add, #doc-forgejo";

// ghostEl are the routines' text (C16), measured as contrastEl is
const ghostEl = ".card.ghost .ref, .card.ghost .ttl, .card.ghost .line2, .card.ghost .held, .card.ghost .run, .card.ghost .acts button, " +
  ".chip.routine, .cell.routine";

// notifyEl are the messenger panel's text (roadmap 145, C19), measured
// as contrastEl is
const notifyEl = ".notify th[scope=row], .notify td, .notify .dim, .notify .addr, .notify caption, .notify td.act a, .notify td.act button, .notify select";

// tokenEl are the chat token's text (C43): the label on a token's row,
// the Tokens section and its shown-once block, measured as contrastEl is
const tokenEl = ".msg .who .via, .msg.tok .who .name, .once-w, .once-l, .once-k, .cline .ps, .cline code, .cline button, .mint > summary, " +
  ".notify.once h3, .notify td, .notify th[scope=row], .notify .dim, .notify caption, .notify td.act button";

// madeToken opens the token form where it folds, makes a token named
// name (desk-claude) and waits for the answer: the page with the
// shown-once block
async function madeToken(page, name = "desk-claude") {
  if (!(await page.locator("#token-name").isVisible())) await page.locator("details:has(#token-name) > summary").click();
  await page.locator("#token-name").fill(name);
  await page.locator("#token-forgejo").fill("lab-forgejo-token");
  await Promise.all([page.waitForNavigation(), page.getByRole("button", { name: "Make chat token" }).click()]);
  await page.waitForTimeout(400);
}

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
  escalation: ["Answer:", "The factory's thread. Answer with its buttons, and write in a thread of your own.", "needs you",
    "autonomous level · started by the agent", "Postgres shares the factory's backups", "Try the failed card again", "Not now"],
  poke: ["Inbox", "Why is the session store card waiting?", "1 needs you · 1 new", "archived", "restore", "Archive", "Delete…"],
  compacted: ["4 earlier messages, compacted", "summary", "#11 waits until the store is settled.",
    "Why is the session store card waiting?"],
  wake: ["Nothing for you. The agent looked at #603 (Board export: keep the nightly snapshots)."],
  stopped: ["Plan the members page", "their logins, roles and", "Stopped by sebastian."],
  thinking: ["Plan the members page"],
  assisted: ["Inbox", "Board", "autonomy", "assisted", "Filed sf/code#8 on track direct.",
    "#7", "#8", "#9", "Speed up the find box", "Fix the login redirect", "Trim the roll notes"],

  busy: ["Factory console", "New task", "Settings", "Resources", "Costs", "Policy", "Notifications", "Log out", "sebastian", "Theme",
    "waiting on you", "3 working", "3 need you", "61.40 of 120.00 EUR", "GitHub", "configuration", "version 4",
    "Needs you", "Queued", "In progress", "Done",
    "Merge", "high", "Approve", "Close",
    "How far back does the feed page by default?",
    "#598", "#616", "#617", "waits for #603", "#533",
    "#606", "1.72 of its 2.00 EUR", "near its budget", "#611", "#614", "#601",
    "#531", "#596", "#600", "#597", "failed", "#590", "closed", "#535", "Retry",
    "project agent", "Start a new thread", "What is #606 stuck on", "Do it", "Skip", "Send",
    "File roadmap 300 as a card", "One card, or one per deliverable", "One card",
    "Refused: The ready check refuses the filing", "0 acceptance tests, want 1 to 10",
    "acceptance the chat shows the documents a member sends; the agent reads them by tool",
    "The failing run is img1", "analyze_image", "In #606's session", "A fresh session",
    "?", "Find on the board", "Find: a word, #612, plan:558",
    "Order and show", "Order", "Show", "newest first", "oldest first", "next up", "by stage", "by spend", "blocked only", "exceptional only", "failed only",
    "↻ routines", "within", "Update the fixed versions of outside software every week", "Template pass", "Dependency update", "Audit", "every Monday 06:00", "after 6 more merges",
    "#779"],
  idle: ["idle", "0 working", "0 need you", "144.16 of 120.00 EUR", "#455", "#178", "Done", "#618", "#597", "failed"],
  fire: ["#610", "LiteLLM backstop hit", "2 waiting on the provider",
    "#620", "#615", "#621", "#604", "#609", "failed", "cx33", "load test of the event stream",
    "#623", "waits for #610", "#602"],
  quiet: ["4 working", "0 need you", "4.10 of 120.00 EUR", "#624", "#625", "#622", "#621", "#620", "done",
    "chain run 3251", "live through run 3248, 25 min after merge"],

  tokens: ["via claude-laptop", "sent by the chat token claude-laptop", "What does #606 still wait on?", "Leave #606 to its session."],
  metrics: ["Delivery", "Stability", "Cost", "Autonomy", "The factory itself", "deploys / week", "lead time, push to live",
    "spend by model", "served by", "7 days", "28 days", "90 days"],
};

// mustOpen are the words a scenario's opened cards show, by the hash that
// opens each, its folds open: an ask's question, facts and answers, and a
// routine's controls, which the small card leaves to its drawer.
const mustOpen = {
  busy: {
    [labID(603)]: ["The agent asks: Which store keeps the nightly board snapshots?", "Postgres", "SQLite", "Ask the author", "Not now"],
    [labID(607)]: ["pull request #612", "Merge “Chat proposals survive a restart: keep them in Postgres”?", "high risk", "Migration", "Merge it", "Send it back", "Reject and close it"],
    [labID(558)]: ["How far back does the feed page by default?", "One day", "One week", "The 500 newest rows", "Answer in my words"],
    [labID(601)]: ["pull request #605"],
    "ai-software-factory-code/weekly-bump": ["Skip the next run", "Run it now", "The routine misses its next run."],
  },
  fire: {
    [labID(619) + ":overview"]: ["Rent a cx33 server for 5 hours at about 0.07 EUR?", "nothing for over 41 min", "Beyond the project's envelope", "Approve it", "Refuse it"],
    [labID(620) + ":overview"]: ["1.95 of 2.00 EUR"],
  },
};

async function seed(s) {
  const resp = await fetch(base + "/lab/scenario/" + s);
  if (resp.status !== 204) throw new Error("GET /lab/scenario/" + s + ": " + resp.status);
  return resp.headers.get("x-lab-drawer") || "";
}

// inboxScenes are the scenes whose story is the inbox: the page lands on
// the human's own thread, and the inbox opens by its row, as a human
// opens it
const inboxScenes = ["escalation", "wake"];
async function openInbox(page) {
  await page.locator("#thread-rows .trow.inbox").click();
  await page.waitForTimeout(600);
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
    {detail: {query: "?project=ai-software-factory-code&queued=oldest&q=costs"}})));
  await page.waitForTimeout(900);
  const at = new URL(page.url()).searchParams;
  if (at.get("q") !== "costs" || at.get("queued") !== "oldest" || !at.get("project")) fails.push(label + ": the URL holds " + page.url());
  const words = await page.locator("#q").inputValue();
  if (words !== "costs") fails.push(label + ": the find's box holds " + JSON.stringify(words) + ", not the event's q");
  if ((await page.evaluate(() => history.length)) !== before + 1) fails.push(label + ": the change leaves no history entry");
  const shown = await page.locator(cards).allInnerTexts();
  if (!shown.length || shown.some((t) => !t.toLowerCase().includes("costs"))) fails.push(label + ": the board shows " + shown.length + " cards that do not say costs");
  await page.goBack({ waitUntil: "load" });
  await page.waitForTimeout(600);
  if (new URL(page.url()).searchParams.get("q")) fails.push(label + ": Back leaves q in the URL (" + page.url() + ")");
  if ((await page.locator(cards).count()) < shown.length + 1) fails.push(label + ": Back does not read the entry's board");
  await page.locator("#status-line > summary > .cell.state").click();
  await page.waitForTimeout(200);
  if (!(await page.locator("#status-panel").isVisible())) fails.push(label + ": the strip does not open the panel");
  if ((await page.locator("#status-line .cell.hold").count()) !== 1) fails.push(label + ": the strip lacks the production hold cell");
  if ((await page.locator("#status-panel form[action$=deploy-now] button", { hasText: "Deploy now" }).count()) !== 1) fails.push(label + ": the panel lacks Deploy now");
  await page.locator("#status-line > summary > .cell.state").click();
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
  await page.keyboard.type("costs", { delay: 40 });
  await page.waitForTimeout(2500);
  await page.unroute(slow, hold);
  const after = await page.evaluate(() => {
    const q = document.getElementById("q"), b = document.getElementById("board");
    return { inBoard: !!q && b.contains(q), box: q && q.value, focus: !!q && document.activeElement === q, query: b.dataset.query, read: window.labRead,
      off: Array.from(b.querySelectorAll("[data-card]")).filter((k) => !k.innerText.toLowerCase().includes("costs")).length };
  });
  if (!after.inBoard || after.box !== "costs" || !after.focus) {
    fails.push(label + ": after a late read the box is " + (after.inBoard ? "in the board" : "gone") + ", text " + JSON.stringify(after.box) + ", focus " + after.focus);
  }
  if (after.query.indexOf("q=costs") < 0 || after.off) fails.push(label + ": after a late read the board's query is " + after.query + ", " + after.off + " cards do not say costs");
  if (after.read !== "settled") fails.push(label + ": the late read's promise is " + after.read);
}

// cardPress presses Merge it on the busy board's merge card (607), its
// drawer open beside it and the answer held 1.2 s, and checks C31: before
// the answer is in the card stands at the top of In progress, busy, no
// button of it live, the drawer's answers locked too, and a second press
// on it, the drawer's Merge it, sends nothing; a refusal puts it back in
// Needs you where it stood, live, the refusal in the flash. Pressed again
// in its drawer and accepted while the factory's board still shows it
// waiting, it keeps its new place and its lock, the drawer's answers
// locked too, through the answer's board; a board that shows it out of
// Needs you lets it go.
async function cardPress(page, fails) {
  const id = labID(607), sel = '#board .cards > [data-card="' + id + '"]';
  await page.goto(base + "/#card=" + id, { waitUntil: "load" });
  await page.waitForSelector("#drawer .o-answers button");
  await page.waitForTimeout(600);
  const where = () => page.evaluate((s) => {
    const k = document.querySelector(s), f = document.getElementById("flash"), o = document.querySelector("#drawer .o-answers");
    return k && { col: k.closest(".column").dataset.holds, at: Array.prototype.indexOf.call(k.parentNode.children, k), busy: k.getAttribute("aria-busy"),
      live: Array.from(k.querySelectorAll("button")).filter((b) => !b.disabled).length, flash: f.hidden ? "" : f.textContent, drawer: o && o.getAttribute("aria-busy") };
  }, sel);
  const before = await where();
  if (!before || before.col.indexOf("waiting-human") < 0 || !before.live || before.drawer) return fails.push("card press: #607 is not a live card in Needs you: " + JSON.stringify(before));
  // the merge is answered through its case's door, whose path the form holds
  const merge = '#drawer .o-answers form[data-answer="approve_merge"]';
  const post = await page.evaluate((m) => { const f = document.querySelector(m); return f && f.getAttribute("action"); }, merge);
  if (!post) return fails.push("card press: the drawer of #607 holds no Merge it form");
  let posts = 0, answer;
  const answers = (u) => u.pathname === post;
  await page.route(answers, async (r) => {
    posts++;
    await new Promise((ok) => setTimeout(ok, 1200));
    await answer(r);
  });
  answer = (r) => r.fulfill({ status: 409, contentType: "text/plain", body: "The lab refuses the answer." });
  await page.locator(sel + ' button:text-is("Merge it")').click();
  const now = await where();
  if (!now || now.col.indexOf("in-review") < 0 || now.at !== 0 || now.busy !== "true" || now.live || now.drawer !== "true") {
    fails.push("card press: right after the press the card stands " + JSON.stringify(now) + ", want the top of In progress, busy, no live button, the drawer's answers locked");
  }
  await page.evaluate((m) => document.querySelector(m).requestSubmit(), merge);
  await page.waitForTimeout(200);
  if (posts !== 1) fails.push("card press: two presses on the card sent " + posts + " answers");
  await page.waitForTimeout(1400);
  const refused = await where();
  if (!refused || refused.col !== before.col || refused.at !== before.at || refused.busy || refused.live !== before.live || refused.drawer || refused.flash !== "The lab refuses the answer.") {
    fails.push("card press: after the refusal the card stands " + JSON.stringify(refused) + ", want back where it stood " + JSON.stringify(before) + " and the refusal in the flash");
  }
  const query = await page.evaluate(() => document.getElementById("board").dataset.query);
  const waiting = await (await page.request.get(base + "/board" + query, { headers: { "HX-Request": "true" } })).text();
  answer = (r) => r.fulfill({ status: 204, headers: { "HX-Trigger": "board-refresh" } });
  posts = 0;
  await page.locator('#drawer .o-answers button:text-is("Merge it")').click();
  await page.waitForTimeout(1800);
  const kept = await where();
  if (posts !== 1 || !kept || kept.col.indexOf("in-review") < 0 || kept.busy !== "true" || kept.live || kept.drawer !== "true") {
    fails.push("card press: accepted from the drawer while the factory's board shows the card waiting, " + posts + " answers sent, the card " + JSON.stringify(kept) +
      ", want one answer, the card in In progress and both locked");
  }
  await page.unroute(answers);
  await page.route((u) => u.pathname === "/board", (r) => r.fulfill({ status: 200, contentType: "text/html", body: waiting.split('data-card="' + id + '"').join('data-card="t-moved"') }));
  await page.evaluate(() => htmx.ajax("GET", "/board" + document.getElementById("board").dataset.query, { source: "#board", target: "#board", swap: "outerHTML" }));
  await page.waitForTimeout(800);
  const gone = await page.evaluate((s) => ({ card: !!document.querySelector(s), busy: document.querySelectorAll("#board [aria-busy], #board [data-sent]").length }), sel);
  if (gone.card || gone.busy) fails.push("card press: a board that shows the card out of Needs you leaves " + JSON.stringify(gone) + ", want the card let go");
}

// brief opens the full brief's fold of the card the drawer shows, where
// its promises stand
const brief = (page) => page.evaluate(() => document.querySelectorAll('#drawer details[data-sec="brief"]').forEach((d) => { d.open = true; }));

// promises are the drawer's promise rows as their first line reads: the
// promise in the plan's words and its state
const promises = (page) => page.evaluate(() => Array.from(document.querySelectorAll("#drawer .criterion > p:first-child")).map((p) => p.textContent.trim()));

// outside are the drawer's promise rows, and their parts, that reach past
// the drawer's edge, and a promise's artifact that scrolls sideways; null
// for a drawer that shows no promise
const outside = (page) => page.evaluate(() => {
  const d = document.querySelector("#drawer");
  if (!d || !d.querySelector(".criterion")) return null;
  const edge = d.getBoundingClientRect().right;
  return Array.from(d.querySelectorAll(".criterion, .criterion *")).filter((e) => e.getBoundingClientRect().right > edge + 1 ||
    (e.matches("pre.artifact") && e.scrollWidth > e.clientWidth)).map((e) => e.tagName.toLowerCase() + (e.className ? "." + e.className : "")).slice(0, 4);
});

// page is the ask the drawer's opened card shows: its label with the page
// it is on, and the story's first line, what the plan promised
const page = (p) => p.evaluate(() => {
  const a = document.querySelector("#drawer .o-ask");
  return a && { label: a.querySelector(".o-label").firstChild.textContent.trim(), planned: (a.querySelector(".o-story li") || { textContent: "" }).textContent.trim() };
});

// held is how the drawer's answers stand: dimmed while a press waits for
// its board, and the buttons still live
const held = (p) => p.evaluate(() => {
  const a = Array.from(document.querySelectorAll("#drawer .o-answers"));
  return { busy: a.filter((e) => e.getAttribute("aria-busy") === "true").length, live: a.reduce((n, e) => n + Array.from(e.querySelectorAll("button")).filter((b) => !b.disabled).length, 0) };
});

// slowBoard holds every board read of p 1.2 s and counts them
async function slowBoard(p) {
  const n = { reads: 0 };
  await p.route((u) => u.pathname === "/board", async (r) => {
    n.reads++;
    await new Promise((ok) => setTimeout(ok, 1200));
    await r.continue();
  });
  return n;
}

// press sends answer of the drawer's opened card, its words typed first
// when it asks for them, and checks its answers dim until the board that
// answers it lands; label names the check
async function press(p, answer, words, label, fails) {
  const board = await slowBoard(p);
  const form = p.locator('#drawer form[data-answer="' + answer + '"]');
  if (words) {
    await form.locator("button.pick").click();
    await form.locator("textarea").fill(words);
    await form.locator("button.send").click();
  } else {
    await form.locator("button.pick").click();
  }
  await p.waitForTimeout(500);
  const mid = await held(p);
  if (!mid.busy || mid.live) fails.push(label + ": before its board lands the card's answers stand " + JSON.stringify(mid) + ", want them dimmed with no live button");
  await p.waitForTimeout(2500);
  if (!board.reads) fails.push(label + ": the press read no board");
  await p.unroute((u) => u.pathname === "/board");
}

// criteriaWaive opens the criteria scene's gap card, plan #1344: its
// promises in the full brief, the reviewer's evidence with one unbroken
// file::test name, stay inside the drawer at 1440 and 390. The card asks
// on a page per open promise. Drop this promise on the second page, with
// its reason, dims the card's answers until the board that answers it
// lands; then the card reads that promise waived, asks on the two pages
// still open, and its answers are live again.
async function criteriaWaive(browser, fails) {
  await seed("criteria");
  const gap = labID(1344);
  for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
    const p = await open(browser, viewport);
    await p.page.goto(base + "/#card=" + gap, { waitUntil: "load" });
    await p.page.waitForTimeout(1200);
    await brief(p.page);
    await p.page.waitForTimeout(200);
    const out = await outside(p.page);
    if (!out) fails.push("criteria waive: the gap card #1344 shows no promise at " + viewport.width);
    else if (out.length) fails.push("criteria waive: at " + viewport.width + " the promises reach past the drawer's edge: " + out.join(", "));
    await p.ctx.close();
  }
  const p = await open(browser, { width: 1440, height: 900 });
  await p.page.goto(base + "/#card=" + gap, { waitUntil: "load" });
  await p.page.waitForTimeout(1200);
  const first = await page(p.page);
  const want1 = { label: "Your call · 1 of 3", planned: "The plan promised: The panel counts the cards over their size bound." };
  if (JSON.stringify(first) !== JSON.stringify(want1)) fails.push("criteria waive: the card opens on " + JSON.stringify(first) + ", want " + JSON.stringify(want1));
  await p.page.locator("#drawer .o-pages a.next").click();
  await p.page.waitForTimeout(800);
  const second = await page(p.page);
  const want2 = { label: "Your call · 2 of 3", planned: "The plan promised: The new panels keep the page's window and its old figures." };
  if (JSON.stringify(second) !== JSON.stringify(want2)) fails.push("criteria waive: the next page is " + JSON.stringify(second) + ", want " + JSON.stringify(want2));
  await press(p.page, "answer_criteria/waive", "The markers, the denial and the range are proved.", "criteria waive", fails);
  await brief(p.page);
  const after = { rows: await promises(p.page), label: (await page(p.page)).label, held: await held(p.page) };
  const want = ["The panel counts the cards over their size bound. partial", "The new panels keep the page's window and its old figures. waived",
    "A retried session spends once in the ledger. partial"];
  if (JSON.stringify(after.rows) !== JSON.stringify(want) || !/ of 2$/.test(after.label) || after.held.busy || !after.held.live) {
    fails.push("criteria waive: once its board landed the card reads " + JSON.stringify(after) + ", want " + JSON.stringify(want) + " on a page of 2 and its answers live");
  }
  await p.ctx.close();
}

// criteriaHuman opens the human scene's merge card of t-int: its promise
// rows in the full brief stay inside the drawer at 1440 and 390, and
// pages_ok's artifact, one unbroken line, wraps without a sideways
// scroll. The card asks first on the page of each human promise with no
// verdict, then on the merge's page, where Merge it stays disabled while
// one waits. It is met on pages_ok's page dims the card's answers until
// the board that answers it lands; the card then reads pages_ok met and
// asks on page_links's page. Drop this promise there, with its reason,
// leaves the merge's page alone, Merge it live.
async function criteriaHuman(browser, fails) {
  await seed("human");
  const merge = (page) => page.evaluate(() => Array.from(document.querySelectorAll("#drawer button.pick")).filter((b) => b.textContent.trim() === "Merge it").map((b) => b.disabled));
  for (const viewport of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
    const p = await open(browser, viewport);
    await p.page.goto(base + "/#card=t-int", { waitUntil: "load" });
    await p.page.waitForTimeout(1200);
    await brief(p.page);
    await p.page.waitForTimeout(200);
    const out = await outside(p.page);
    if (!out) fails.push("criteria human: the merge card of t-int shows no promise at " + viewport.width);
    else if (out.length) fails.push("criteria human: at " + viewport.width + " the promises reach past the drawer's edge: " + out.join(", "));
    await p.ctx.close();
  }
  const p = await open(browser, { width: 1440, height: 900 });
  await p.page.goto(base + "/#card=t-int", { waitUntil: "load" });
  await p.page.waitForTimeout(1200);
  await brief(p.page);
  const rows = ["Every size row stays under its bound. met (blind)", "The sizes page reads calm. met", "The tests in ./... stay green. unmet",
    "The pages read well. open", "Every page links home. open"];
  const before = { rows: await promises(p.page), page: await page(p.page) };
  const want = { rows, page: { label: "Your call · 1 of 3", planned: "The plan promised: The pages read well." } };
  if (JSON.stringify(before) !== JSON.stringify(want)) fails.push("criteria human: the card reads " + JSON.stringify(before) + ", want " + JSON.stringify(want));
  for (const _ of [1, 2]) {
    await p.page.locator("#drawer .o-pages a.next").click();
    await p.page.waitForTimeout(800);
  }
  const last = { label: (await page(p.page)).label, merge: await merge(p.page) };
  if (JSON.stringify(last) !== JSON.stringify({ label: "Your call · 3 of 3", merge: [true] })) {
    fails.push("criteria human: the merge's page reads " + JSON.stringify(last) + ", want the third of 3 with Merge it disabled");
  }
  await p.page.reload({ waitUntil: "load" });
  await p.page.waitForTimeout(1200);
  await press(p.page, "answer_criteria", "", "criteria human, It is met", fails);
  await brief(p.page);
  rows[3] = "The pages read well. met";
  const met = { rows: await promises(p.page), page: await page(p.page) };
  const wantMet = { rows, page: { label: "Your call · 1 of 2", planned: "The plan promised: Every page links home." } };
  if (JSON.stringify(met) !== JSON.stringify(wantMet)) fails.push("criteria human: after It is met the card reads " + JSON.stringify(met) + ", want " + JSON.stringify(wantMet));
  await press(p.page, "answer_criteria/waive", "The help page is gone.", "criteria human, Drop this promise", fails);
  await brief(p.page);
  rows[4] = "Every page links home. waived";
  const waived = { rows: await promises(p.page), merge: await merge(p.page) };
  if (JSON.stringify(waived) !== JSON.stringify({ rows, merge: [false] })) {
    fails.push("criteria human: after the drop the card reads " + JSON.stringify(waived) + ", want " + JSON.stringify({ rows, merge: [false] }));
  }
  await p.ctx.close();
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
    if (inboxScenes.includes(sc)) {
      if ((await p.page.locator("#thread-head").innerText()).includes("the factory's thread")) fails.push(sc + ": the page lands on the inbox, not a thread of the human's own");
      // a palette left open in the human's own thread stays behind
      if (sc === "escalation") { await p.page.locator("#chat-input").click(); await p.page.keyboard.type("/"); }
      await openInbox(p.page);
      if (sc === "escalation" && (await p.page.locator("#chat-palette").isVisible())) fails.push("inbox: the command palette follows into the inbox");
    }
    // the status in detail opens on its chevron: its words count too
    const xp = p.page.locator("#status-toggle");
    if (await xp.count()) { await xp.click(); await p.page.waitForTimeout(200); }
    const low = (await text(p.page)).toLowerCase();
    if (await xp.count()) { await p.page.keyboard.press("Escape"); await p.page.waitForTimeout(200); }
    for (const w of words) if (!low.includes(w.toLowerCase())) fails.push(sc + ': missing "' + w + '"');
    for (const [at, want] of Object.entries(mustOpen[sc] || {})) {
      const op = await open(browser, { width: 1440, height: 900 });
      await op.page.goto(base + "/#" + (at.startsWith("t-") ? "card=" : "routine=") + at, { waitUntil: "load" });
      await op.page.waitForTimeout(800);
      await op.page.evaluate(() => document.querySelectorAll("#drawer details").forEach((d) => { d.open = true; }));
      const got = (await op.page.locator("#drawer").innerText()).toLowerCase();
      for (const w of want) if (!got.includes(w.toLowerCase())) fails.push(sc + ": the drawer of " + at + ' lacks "' + w + '"');
      await op.ctx.close();
    }
    if (sc === "tokens") {
      // a token's rows (C43): the dashed avatar and "via" with the token's
      // name on each, nothing on the row the browser sent
      const rows = await p.page.evaluate(() => Array.from(document.querySelectorAll("#chat-log .msg.you")).map((m) => ({
        tok: m.classList.contains("tok"), via: (m.querySelector(".who .via") || {}).textContent || "",
        ring: getComputedStyle(m.querySelector(".av")).borderTopStyle, said: m.querySelector(".said").textContent })));
      const want = JSON.stringify([{ tok: true, via: "via claude-laptop", ring: "dashed", said: "What does #606 still wait on?" },
        { tok: true, via: "via claude-laptop", ring: "dashed", said: "Which line of the test fails?" },
        { tok: false, via: "", ring: "solid", said: "Leave #606 to its session. I look at it after lunch." }]);
      if (JSON.stringify(rows) !== want) fails.push("tokens: the human rows read " + JSON.stringify(rows) + ", want " + want);
    }
    if (sc === "stopped") {
      // the stopped turn (the 2026-10-31 stop plan): the cut answer keeps
      // one grey stopped mark that is no button, and the factory's stop
      // row names the member
      if ((await p.page.locator("#chat-log .thinking.stopped").count()) !== 1) fails.push("stopped: the cut answer carries no grey stopped mark");
      if (await p.page.locator("#chat-log button.thinking").count()) fails.push("stopped: the stopped mark is still a button");
      if (!(await p.page.locator("#chat-log .msg .said").filter({ hasText: "Stopped by sebastian." }).count())) fails.push("stopped: the factory's stop row is missing");
    }
    if (sc === "thinking") {
      // the live fold (the thinking-fold plan): a turn in flight carries
      // the chevron beside the stop mark, a press opens the reasoning
      // while the stream runs, and the reasoning never lands: once the
      // turn ends and the page reloads, no fold and no chevron remain
      await p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/model-line-slow?ms=300");
      await p.page.locator("#chat-input").click();
      await p.page.keyboard.type("what is open?");
      await p.page.keyboard.press("Enter");
      await p.page.locator("#chat-log [data-think]").waitFor({ state: "visible" });
      if ((await p.page.locator("#chat-log .thinking[data-stop]").count()) !== 1) fails.push("thinking: the reasoning turn carries no stop mark");
      await p.page.locator("#chat-log [data-think]").click();
      await p.page.waitForTimeout(700);
      const openFold = await p.page.evaluate(() => {
        const chev = document.querySelector("#chat-log [data-think]"), fold = document.querySelector("#chat-log .tfold");
        return { open: chev && chev.getAttribute("aria-expanded") === "true", shown: fold && !fold.hidden, text: fold && fold.textContent || "" };
      });
      if (!openFold.open || !openFold.shown) fails.push("thinking: a press on the chevron does not open the fold");
      if (!/The member asks what is open/.test(openFold.text)) fails.push("thinking: the open fold does not carry the live reasoning");
      await p.page.locator("#chat-log .thinking[data-stop]").waitFor({ state: "detached", timeout: 15000 });
      await p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/model-line-slow?ms=0");
      await p.page.waitForTimeout(400);
      if (!(await p.page.locator("#chat-log .said").filter({ hasText: "Two cards are open." }).count())) fails.push("thinking: the turn's answer does not land");
      await p.page.reload({ waitUntil: "load" });
      await p.page.waitForTimeout(700);
      if (await p.page.locator("#chat-log [data-think], #chat-log .tfold").count()) fails.push("thinking: the fold or its chevron survives the reload");
      if (!(await p.page.locator("#chat-log .said").filter({ hasText: "Two cards are open." }).count())) fails.push("thinking: the answer does not survive the reload");
    }
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
      const word = merged.locator(".foot .st a");
      if ((await word.count()) !== 1 || (await word.innerText()).trim() !== "blocked by roll #626") fails.push("quiet: the merged card's face does not read the linked word blocked by roll #626");
      else if ((await word.getAttribute("href")) !== "#card=" + labID(626)) fails.push("quiet: the roll word links " + (await word.getAttribute("href")) + ", not the roll card's drawer");
      else {
        await word.click();
        await p.page.waitForTimeout(600);
        if (!(await p.page.locator("#drawer").innerText()).includes("roll 5e1f0a2")) fails.push("quiet: a click on the roll word does not open the roll card's drawer");
        await p.page.keyboard.press("Escape");
        await p.page.waitForTimeout(300);
      }
      const rollCol = await p.page.locator('#board section:has(h3:has-text("In progress")) [data-card="' + labID(626) + '"]').count();
      if (!rollCol) fails.push("quiet: the roll card is not in the In progress column");
      // the chain's steps on the card (round 15): the carried card's face
      // names its run and links the forge, the Activity tab reads the chain
      // behind it collapsed to its summary, and the covered done face
      // carries the cover clause
      const carrier = p.page.locator('#board [data-card="' + labID(625) + '"]');
      const runWord = carrier.locator(".foot .st a");
      if ((await runWord.count()) !== 1 || (await runWord.innerText()).trim() !== "chain run 3251") fails.push("quiet: the carried card's face does not read the linked chain word chain run 3251");
      else if ((await runWord.getAttribute("href")) !== "https://git.example/sf/code/actions/runs/3251") fails.push("quiet: the chain word links " + (await runWord.getAttribute("href")) + ", not the run on the forge");
      else {
        await carrier.click({ position: { x: 4, y: 4 } });
        await p.page.waitForTimeout(600);
        const d = p.page.locator("#drawer");
        await d.getByRole("tab", { name: "Activity" }).click();
        await p.page.waitForTimeout(400);
        const chain = d.locator('details[data-sec="chain"]');
        if (!(await chain.count())) fails.push("quiet: the Activity tab shows no Chain section");
        else {
          if (await chain.getAttribute("open")) fails.push("quiet: the Chain section stands open while the chain is quiet");
          if (!(await chain.locator("summary").innerText()).includes("run 3251 pending")) fails.push("quiet: the Chain summary's hint does not carry the live fact");
          await chain.locator("summary").click();
          await p.page.waitForTimeout(300);
          const ct = await chain.innerText();
          for (const w of ["carry run", "factory/roll", "run 3260", "green"]) if (!ct.includes(w)) fails.push('quiet: the Chain readout lacks "' + w + '"');
          if (!(await chain.locator('a[href="https://git.example/sf/code/actions/runs/3251"]').count())) fails.push("quiet: the carry run does not link the forge");
        }
        await p.page.keyboard.press("Escape");
        await p.page.waitForTimeout(300);
      }
      if (!(await p.page.locator('#board [data-card="' + labID(622) + '"]').innerText()).includes("live through run 3248, 25 min after merge")) fails.push("quiet: the covered done face lacks its cover clause");
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
          const tabs = await d.getByRole("tab").allInnerTexts();
          if (tabs.join(", ") !== "Overview, Session, Activity, Files, Receipt") fails.push("drawer: the tabs read " + tabs.join(", ") + ", want Overview, Session, Activity, Files, Receipt");
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
      // form out of the page and puts it back. Its word, costs, is on the face of each card
      // it finds: a face in Needs you shows its ask, not the title the find reads too
      await findAsYouType(p, "?queued=oldest&routines=within&within=2&unit=hours", "costs", "find as you type", fails);
      // the agent's view change (282) on the same board: the page stands under the
      // order's and the routines' menu choices, and the event's query carries the
      // find and the order, so the merge into the URL reads plainly
      await viewChange(p, "?queued=oldest&routines=within&within=2&unit=hours", fails);
      const nm = await open(browser, { width: 1440, height: 900 });
      await nm.ctx.addInitScript(() => { delete Element.prototype.moveBefore; });
      await nm.page.goto(base + "/", { waitUntil: "load" });
      if (await nm.page.evaluate(() => "moveBefore" in Element.prototype)) fails.push("find without moveBefore: the lab could not take moveBefore away");
      else await findAsYouType(nm, "", "costs", "find without moveBefore", fails);
      if (nm.errors.length) fails.push("find without moveBefore: " + nm.errors.join("; "));
      await nm.ctx.close();
      // a find keeps the open drawer's hash in the URL it writes, so the drawer's ✕ still closes it;
      // the drawer at its Overview, as its Chat, read again when the stream moves, writes its hash anew
      await p.page.goto(base + "/#card=" + drawer + ":overview", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      await p.page.locator("#q").focus();
      await p.page.keyboard.type("costs", { delay: 40 });
      await p.page.waitForTimeout(1200);
      const found = new URL(p.page.url());
      if (found.hash !== "#card=" + drawer || found.searchParams.get("q") !== "costs") fails.push("find with a drawer: the find writes " + p.page.url() + ", want q=costs and #card=" + drawer);
      await p.page.locator("#drawer-close").click();
      await p.page.waitForTimeout(400);
      const closed = new URL(p.page.url());
      if (await p.page.locator("#drawer").isVisible()) fails.push("find with a drawer: the ✕ does not close the drawer after a find");
      if (closed.hash || closed.searchParams.get("q") !== "costs") fails.push("find with a drawer: after the ✕ the URL is " + p.page.url() + ", want q=costs and no hash");
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
      await p.page.keyboard.type("costs", { delay: 40 });
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
      await p.page.keyboard.type("costs", { delay: 40 });
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
      if (stay.q !== "costs" || stay.hash || stay.box !== "costs" || stay.cards !== narrowed || !stay.same) {
        fails.push("find then drawer: after Back the URL's q is " + stay.q + ", the hash " + JSON.stringify(stay.hash) + ", the box " + JSON.stringify(stay.box) + ", " +
          stay.cards + " of " + narrowed + " cards, the page " + (stay.same ? "kept" : "loaded again"));
      }
      // a stream swap that lands while a find is in flight leaves the find's answer on the board
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const slow = (u) => u.pathname === "/" && u.searchParams.get("q") === "costs";
      await p.page.route(slow, async (r) => { await new Promise((ok) => setTimeout(ok, 1000)); await r.continue(); });
      await p.page.locator("#q").click();
      await p.page.keyboard.type("costs", { delay: 40 });
      await p.page.waitForTimeout(500);
      await p.page.evaluate(() => htmx.ajax("GET", "/board" + document.getElementById("board").dataset.query, { source: "#board", target: "#board", swap: "outerHTML" }));
      await p.page.waitForTimeout(2000);
      await p.page.unroute(slow);
      const raced = await p.page.evaluate(() => ({ query: document.getElementById("board").dataset.query,
        off: Array.from(document.querySelectorAll("#board [data-card]")).filter((k) => !k.innerText.toLowerCase().includes("costs")).length }));
      if (raced.query.indexOf("q=costs") < 0 || raced.off) fails.push("find in a race: after a stream swap landed first the board's query is " + raced.query + ", " + raced.off + " cards do not say costs");
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
      const brief = p.page.locator('#drawer form[data-answer="edit_brief"]');
      if (!(await brief.count())) fails.push("drawer refusal: the drawer has no Edit the brief");
      else {
        const refuse = (u) => u.pathname.endsWith("/resolve");
        await p.page.route(refuse, async (r) => {
          await new Promise((ok) => setTimeout(ok, 1000));
          await r.fulfill({ status: 422, contentType: "text/plain", body: "Refused: not yours." });
        });
        await brief.evaluate((f) => f.requestSubmit());
        await p.page.waitForTimeout(200);
        await p.page.evaluate(() => htmx.ajax("GET", "/board" + document.getElementById("board").dataset.query, { source: "#board", target: "#board", swap: "outerHTML" }));
        await p.page.waitForTimeout(1500);
        await p.page.unroute(refuse);
        // the refusal is the step's own, not the console's
        const own = p.errors.findIndex((x) => x.endsWith("/resolve 422"));
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
      // a plan's name: the place of the children, of the plans and of
      // the plan's own card in Needs you; a child's place links its plan
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const placeOf = (id) => p.page.locator('#board .cards > [data-card="' + labID(id) + '"] .foot .place');
      for (const [card, want] of [[606, "card chips"], [617, "card chips"], [533, "card chips"], [537, "plan quota guard"], [531, "plan card chips"], [527, "plan"]]) {
        const got = (await placeOf(card).allInnerTexts()).map((t) => t.trim());
        if (got.length !== 1 || got[0] !== want) fails.push("plan name: card #" + card + "'s place reads " + JSON.stringify(got) + ", want " + JSON.stringify(want));
      }
      for (const child of [606, 617, 533]) {
        const href = await placeOf(child).locator("a").getAttribute("href").catch(() => null);
        if (href !== "#card=" + labID(531)) fails.push("plan name: card #" + child + "'s place links " + href + ", not plan #531's drawer");
      }
      // the plan's own card in Needs you names the plan in its band
      const place = (await p.page.locator('#needs-you [data-card="' + labID(558) + '"] .band .place').allInnerTexts()).map((t) => t.trim());
      if (place.length !== 1 || place[0] !== "plan activity feed, 4 cards") fails.push("plan name: card #558's place in Needs you reads " + JSON.stringify(place) + ", want \"plan activity feed, 4 cards\"");
    }
    if (sc === "busy") {
      // the routines (C16): every ghost at the foot of Queued, Needs you holds only the
      // filed card (the 2026-10-17 ruling), the control in the
      // board's head, the strip's cell, the chip, the routine's drawer and its skip
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
      const routine779 = (await p.page.locator('#board [data-card="' + labID(779) + '"] .foot .place').allInnerTexts()).map((t) => t.trim());
      if (routine779.join() !== "↻ dependency-update") fails.push("routines: #779's place in Done reads " + JSON.stringify(routine779) + ", want ↻ dependency-update");
      const d = p.page.locator("#drawer");
      await p.page.locator('#board [data-routine="ai-software-factory-code/weekly-bump"]').click();
      await p.page.waitForTimeout(600);
      if (!(await d.isVisible())) fails.push("routines: a click on the ghost does not open the drawer");
      else {
        const tabs = await d.getByRole("tab").allInnerTexts();
        if (tabs.join(",") !== "Overview,Runs,Config") fails.push("routines: the drawer's tabs are " + tabs.join(", "));
        const title = (await d.locator("h2").innerText()).trim();
        if (title !== "↻ Update the fixed versions of outside software every week") fails.push('routines: the drawer\'s title is "' + title + '"');
        await p.page.keyboard.press("Escape");
        await p.page.waitForTimeout(300);
        if (await d.isVisible()) fails.push("routines: Escape does not close the routine's drawer");
      }
      // the Runs tab links each run's card, #781 the newest
      await p.page.goto(base + "/#routine=ai-software-factory-code/weekly-bump:runs", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      if (!(await d.locator('a[href="#card=' + labID(781) + '"]').count())) fails.push("routines: the Runs tab does not link #781");
      // "Skip the next run" is the routine's opened card's answer: the ghost
      // then reads skipped once, and the drawer offers the undo
      await p.page.goto(base + "/#routine=ai-software-factory-code/weekly-bump", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      await d.getByRole("button", { name: "Skip the next run" }).click();
      await p.page.waitForTimeout(800);
      const skipped = await p.page.locator('#board [data-ghost="ai-software-factory-code/weekly-bump"]').innerText();
      if (!skipped.includes("skipped once")) fails.push("routines: after the skip the ghost reads " + JSON.stringify(skipped) + ", want skipped once");
      if (!(await d.getByRole("button", { name: "Undo the skip" }).count())) fails.push("routines: after the skip the drawer offers no Undo the skip");
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
      // the held run (C16): the ghost and the strip's cell in amber, #781 in Needs you with its routine as its place
      if (!(await p.page.locator("#board .card.ghost.held").count())) fails.push("fire: no held ghost");
      const cell = await p.page.locator(".strip .cell.routine").allInnerTexts();
      if (!cell.includes("↻ bump held")) fails.push('fire: the strip\'s cell reads "' + cell.join(" | ") + '", want ↻ bump held');
      const k781 = p.page.locator('#needs-you [data-card="' + labID(781) + '"]');
      const place781 = (await k781.locator(".band .place").allInnerTexts()).map((t) => t.trim());
      if (place781.join() !== "↻ weekly-bump") fails.push("fire: #781's place in Needs you reads " + JSON.stringify(place781) + ", want ↻ weekly-bump");
      // #781's life names the run that filed it and links its routine; the menus text() opened close first
      await p.page.evaluate(() => document.querySelectorAll("details.col-menu").forEach((m) => { m.open = false; }));
      await k781.click({ position: { x: 4, y: 4 } });
      await p.page.waitForTimeout(600);
      const d = p.page.locator("#drawer");
      await p.page.evaluate(() => document.querySelectorAll('#drawer details[data-sec="life"]').forEach((l) => { l.open = true; }));
      const prov = (await d.locator(".provenance").allInnerTexts()).map((t) => t.trim());
      if (prov.join() !== "filed by the routine Update the fixed versions of outside software every week, run 14, last run held") {
        fails.push('fire: #781\'s life reads ' + JSON.stringify(prov) + ', want "filed by the routine Update the fixed versions of outside software every week, run 14, last run held"');
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
        if (await p.page.locator(".arch-note [data-restore-open]").isDisabled()) fails.push("threads: the restore line's restore cannot be pressed");
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
      // the stop button (the 2026-10-31 stop plan): the working thread's
      // mark is the Stop button, drawn from the thread's working flag with
      // no stream open on this page
      await p.page.locator('#thread-rows .trow[data-thread="3"]').click();
      await p.page.waitForTimeout(600);
      const mark = p.page.locator("#chat-log button.thinking[data-stop]");
      if (!(await mark.count())) fails.push("threads: the working thread's mark is no Stop button");
      else if ((await mark.getAttribute("title")) !== "Stop") fails.push("threads: the mark's title reads " + (await mark.getAttribute("title")));
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
      // the inbox is the factory's thread: no box, no chips and no pool
      // control, only its note with the way to a thread of the human's
      // own; "/" puts the focus nowhere
      if (await p.page.locator("#chat-input").isVisible()) fails.push("inbox: the text box shows");
      if (await p.page.locator("#chat-form .cmd").first().isVisible()) fails.push("inbox: the command chips show");
      if (await p.page.locator("[data-pool-ctl]").count()) fails.push("inbox: the pool control shows");
      const note = p.page.locator("#chat-form .inbox-note");
      if (!(await note.isVisible())) fails.push("inbox: no note in the box's place");
      else if (!(await note.innerText()).includes("The factory's thread. Answer with its buttons, and write in a thread of your own.")) fails.push('inbox: the note reads "' + (await note.innerText()) + '"');
      if (!(await p.page.locator("#thread-head").innerText()).includes("the factory's thread: it speaks here uninvited, you answer with its buttons")) fails.push("inbox: the head does not say the human answers with its buttons");
      if (await p.page.locator(".chat-start").isVisible()) fails.push('inbox: the intro line "Ask or tell the project agent" shows');
      await p.page.evaluate(() => document.activeElement && document.activeElement.blur());
      await p.page.keyboard.press("/");
      if (await p.page.evaluate(() => document.activeElement && document.activeElement.id === "chat-input")) fails.push('inbox: "/" puts the focus in the hidden box');
      // a file pasted or dropped on the inbox's form rides nothing: ride
      // pastes into the box or drops on the form a text file and answers
      // how many chips ride the next message
      const ride = (kind) => p.page.evaluate((kind) => {
        const dt = new DataTransfer();
        dt.items.add(new File(["a note"], "note.txt", { type: "text/plain" }));
        const ev = kind === "paste" ? new ClipboardEvent("paste", { clipboardData: dt, bubbles: true, cancelable: true })
          : new DragEvent("drop", { dataTransfer: dt, bubbles: true, cancelable: true });
        document.getElementById(kind === "paste" ? "chat-input" : "chat-form").dispatchEvent(ev);
        return document.querySelectorAll("#chat-images li, #chat-docs li").length;
      }, kind);
      for (const kind of ["paste", "drop"]) {
        const n = await ride(kind);
        if (n) fails.push("inbox: a " + kind + " on the inbox's form rides " + n + " chips");
      }
      // a card's row: its body opens the card's drawer
      const failedRow = p.page.locator('#chat-log .msg.poke[data-card="' + labID(597) + '"]');
      const leave = failedRow.getByRole("button", { name: "Not now", exact: true });
      if (!(await leave.isVisible())) fails.push("inbox: the failed card's poke offers no Not now");
      else {
        await failedRow.locator(".said").click();
        await p.page.waitForTimeout(300);
        const hash = decodeURIComponent(await p.page.evaluate(() => location.hash));
        if (hash !== "#card=" + labID(597)) fails.push('inbox: the failed card\'s row opens no drawer, hash "' + hash + '"');
        await p.page.keyboard.press("Escape");
        await p.page.waitForTimeout(300);
        // a poke's choice is a press through /answer, never a typed
        // message: the choice lands as the human's row and the agent's
        // turn answers it. A poke read once settles at the next read, so
        // the press comes before the answer row's, which reads the thread again
        const posts = [];
        p.page.on("request", (r) => { if (r.method() === "POST") posts.push(new URL(r.url()).pathname); });
        await leave.click();
        await p.page.locator("#chat-log .said").filter({ hasText: "#597 stays failed" }).waitFor({ timeout: 8000 }).catch(() => fails.push("inbox: the agent's turn on the press does not land"));
        if (!posts.includes("/projects/ai-software-factory-code/chat/answer")) fails.push("inbox: the press posts " + JSON.stringify(posts) + ", not to /chat/answer");
        if (posts.includes("/projects/ai-software-factory-code/chat")) fails.push("inbox: the press posts a typed message");
        const said = await p.page.locator("#chat-log .msg.you .said").allInnerTexts();
        if (said[said.length - 1] !== "Not now") fails.push("inbox: the press lands as " + JSON.stringify(said.slice(-1)) + ", want the human's row Not now");
        if (await failedRow.locator(".choices").count()) fails.push("inbox: the pressed poke keeps its choices");
      }
      // the answer row (C14): the pending escalation's choices once, between the log and the box;
      // a choice answers its card with no model turn and the row goes.
      // A settled ask sits over the escalation: the row still offers the
      // escalation, once, not the ask's buttons and not silence.
      const row = p.page.locator("#chat-answer");
      const once = await p.page.locator("#chat").getByRole("button", { name: "Postgres", exact: true }).count();
      if (once !== 1) fails.push("escalation: the chat shows the choice Postgres " + once + " times, want once");
      if (!(await row.isVisible())) fails.push("escalation: no answer row under the inbox's escalation");
      else {
        if ((await row.innerText()).includes("in your words")) fails.push("inbox: the answer row offers words the inbox does not take");
        await row.getByRole("button", { name: "Postgres" }).click();
        await p.page.waitForTimeout(800);
        if (await row.isVisible()) fails.push("escalation: the answer row stays after a choice");
        // under the human's choice, the factory's row on #603 with what came of it
        const tail = await p.page.evaluate(() => Array.from(document.querySelectorAll("#chat-log .msg")).slice(-2).map((m) =>
          ({ you: m.classList.contains("you"), poke: m.classList.contains("poke"), card: m.dataset.card || "", said: (m.querySelector(".said") || m).innerText.trim() })));
        const [you, factory] = tail;
        if (!you || !you.you || you.said !== "Postgres" || !factory || !factory.poke || factory.card !== labID(603) || !factory.said || factory.said.startsWith("The answer did not land"))
          fails.push("escalation: the factory's row on the answer is missing: " + JSON.stringify(tail));
      }
      // the note's New thread opens a thread of the human's own, the box back and in focus
      if (await note.isVisible()) {
        await note.getByRole("button", { name: "New thread" }).click();
        await p.page.waitForTimeout(800);
        if (!(await p.page.locator("#thread-head").innerText()).trim().startsWith("New thread")) fails.push("inbox: the note's New thread opens no new thread");
        if (!(await p.page.locator("#chat-input").isVisible())) fails.push("inbox: the new thread has no text box");
        if (await note.isVisible()) fails.push("inbox: the note stays in the new thread");
        if (await p.page.evaluate(() => document.activeElement && document.activeElement.id) !== "chat-input") fails.push("inbox: after New thread the focus is not in the box");
        if (!(await p.page.locator(".chat-start").isVisible())) fails.push("inbox: the intro line stays hidden in the human's own thread");
        // the same paste and drop ride a thread of the human's own, so
        // the inbox's nothing is the guard's, not a dead event
        const rode = [await ride("paste"), await ride("drop")];
        if (rode.join() !== "1,2") fails.push("inbox: a paste and a drop in the new thread ride " + rode.join(" then ") + " chips, want 1 then 2");
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
      if (!(await p.page.locator("#thread-head").innerText()).includes("the factory's thread: it speaks here uninvited, you answer with its buttons")) fails.push("poke: the inbox's head does not say it is the factory's thread");
      const log = await p.page.locator("#chat").innerText();
      for (const w of ["is done and live", "needs you", "#606", "only a human merge it", "moved on ·"]) if (!log.includes(w)) fails.push('poke: the inbox shows no "' + w + '"');
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
      await logEl.evaluate((e) => { e.style.cssText += ";flex:none;min-height:0;height:120px"; });
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
      // one wake turn is one row (the main agent concept): it folds to one
      // line, what the turn means for the human and the card it looked at;
      // the note and the turn sit behind the fold, a click opens the whole
      // turn and the fold says which state it is in
      const row = p.page.locator("#chat-log .msg.wake");
      if ((await row.count()) !== 1) fails.push("wake: " + (await row.count()) + " wake rows, want 1");
      else {
        const fold = row.locator("[data-fold]");
        if (!(await fold.count())) fails.push("wake: the row carries no fold");
        else {
          if (!(await row.locator(".wake-b").count())) fails.push("wake: the row carries no expandable body");
          const line = (await fold.innerText()).replace(/\s+/g, " ").trim();
          if (line !== "Nothing for you. The agent looked at #603 (Board export: keep the nightly snapshots).") fails.push('wake: the fold reads "' + line + '"');
          if ((await row.innerText()).includes("Wake: 1 ask waits")) fails.push("wake: the folded row shows the wake's note");
          if ((await row.innerText()).includes("Both stores hold the snapshots")) fails.push("wake: the folded row shows the agent's answer");
          if ((await fold.getAttribute("aria-expanded")) !== "false") fails.push("wake: the fold does not say closed");
          await fold.click();
          await p.page.waitForTimeout(150);
          const open = await row.innerText();
          for (const w of ["Wake: 1 ask waits for a human: #603.", "Which store keeps the nightly board snapshots?", "Both stores hold the snapshots"]) if (!open.includes(w)) fails.push('wake: the opened row hides "' + w + '"');
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
  // the caps (C47): at the desk the busy strip is one slim row with the
  // day, its reset's ↺, and the month;
  // the panel's Reset opens its confirm, which names the money and stays
  // open through the board's refreshes, and Cancel closes it; on the
  // phone the press is 44 px high
  await seed("busy");
  for (const vp of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
    const label = "reset at " + vp.width;
    const rp = await open(browser, vp);
    await rp.page.goto(base + "/", { waitUntil: "load" });
    await rp.page.waitForTimeout(600);
    if (vp.width === 1440) {
      const strip = await rp.page.evaluate(() => {
        const cells = Array.from(document.querySelectorAll("#status-line > summary > .cell")), box = (c) => c.getBoundingClientRect();
        const spend = cells.filter((c) => c.classList.contains("spend"));
        return { spend: spend.map((c) => c.textContent.replace(/\s+/g, " ").trim()), rows: new Set(cells.map((c) => box(c).top)).size,
          tall: cells.filter((c) => box(c).height > 40).map((c) => c.textContent.trim().slice(0, 30)) };
      });
      if (strip.spend.length !== 2 || !/↺ today 17\.00 \/ 20\.00 EUR/.test(strip.spend[0]) || !/^61\.40 \/ 120\.00 EUR/.test(strip.spend[1])) fails.push(label + ": the strip lacks the day's reset or the month: " + strip.spend.join(" | "));
      if (strip.rows !== 1 || strip.tall.length) fails.push(label + ": the busy strip takes " + strip.rows + " rows, cells taller than one line: " + strip.tall.join(", "));
    }
    await rp.page.locator("#status-line > summary > .cell.state").click();
    const rst = rp.page.locator("#status-panel details.rst").first();
    if (!(await rst.count())) {
      fails.push(label + ": the panel holds no Reset");
      await rp.ctx.close();
      continue;
    }
    const press = rst.locator("summary");
    if (vp.width === 390 && (await press.boundingBox()).height < 43.5) fails.push(label + ": the Reset press is under 44 px");
    await press.click();
    await rp.page.evaluate(() => document.activeElement.blur());
    await rp.page.waitForTimeout(2500);
    const form = rst.locator("form");
    if (!(await form.isVisible())) fails.push(label + ": the press opens no confirm, or a refresh closed it");
    else if (!/Reset: up to \d+\.\d\d EUR more/.test(await form.innerText())) fails.push(label + ": the confirm lacks Reset: up to");
    await rst.locator("button[data-cancel]").click();
    await rp.page.waitForTimeout(200);
    if (await form.isVisible()) fails.push(label + ": Cancel leaves the confirm open");
    await rp.ctx.close();
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
    const stacked = await pp.page.evaluate(() => Array.from(document.querySelectorAll('[aria-labelledby="connections"] tbody tr, [aria-labelledby="levels"] tbody tr')).map((tr) => getComputedStyle(tr).display));
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
    if (inboxScenes.includes(sc)) await openInbox(pp.page);
    const small = await pp.page.evaluate(() => Array.from(document.querySelectorAll(
      ".trow, .msg .choices button, .tacts button, .cmd, .restore, #level-btn, .open-arch, .swtg, .sw-sum, .answer button, .inbox-note button, " +
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
  // the chat token (C43): a 64-character name cuts with an ellipsis on the
  // phone and keeps the row's time in view; the Tokens section lights the
  // used token's lamp and leaves the unused one dark, the form folds once
  // a token exists; the answer to the form shows the block on the first
  // screen at the desk and on the phone, its Copy puts the line on the
  // clipboard, the stacked rows name their fields, targets reach 44 px
  await seed("tokens");
  {
    const lp = await open(browser, { width: 390, height: 844 });
    await lp.page.goto(base + "/", { waitUntil: "load" });
    await lp.page.waitForTimeout(600);
    const long = await lp.page.evaluate(() => {
      const log = document.getElementById("chat-log");
      log.insertAdjacentHTML("beforeend", chatView.userHTML("sebastian", "a long name", new Date(), false, [], [], "t".repeat(64)));
      const who = log.lastElementChild.querySelector(".who"), via = who.querySelector(".via"), time = who.querySelector("time");
      return { cut: via.scrollWidth > via.clientWidth && getComputedStyle(via).textOverflow === "ellipsis",
        timeIn: time.getBoundingClientRect().right <= who.getBoundingClientRect().right + 0.5 };
    });
    if (!long.cut || !long.timeIn) fails.push("tokens: a 64-character token name at 390 " + JSON.stringify(long) + ", want it cut and the time in the row");
    const lsw = await sideways(lp.page);
    if (lsw) fails.push("tokens: horizontal scroll at 390 with a 64-character token name (" + lsw + " px)");
    await lp.ctx.close();
  }
  for (const vp of [{ width: 1440, height: 900 }, { width: 390, height: 844 }]) {
    await seed("tokens");
    const tp = await open(browser, vp);
    await tp.ctx.grantPermissions(["clipboard-read", "clipboard-write"], { origin: base });
    await tp.page.goto(base + "/notifications", { waitUntil: "load" });
    await tp.page.waitForTimeout(400);
    const w = vp.width;
    const lamps = await tp.page.evaluate(() => Array.from(document.querySelectorAll('[aria-labelledby="tokens"] tbody tr')).map((tr) =>
      tr.querySelector("th").textContent + ":" + ((tr.querySelector(".lamp") || {}).className || "none")));
    if (JSON.stringify(lamps) !== JSON.stringify(["claude-laptop:lamp on", "nightly-ci:lamp"])) fails.push(w + ": the token rows' lamps read " + JSON.stringify(lamps));
    if (await tp.page.locator("#token-name").isVisible()) fails.push(w + ": the token form stands open while tokens exist");
    const projects = await tp.page.evaluate(() => Array.from(document.querySelectorAll('[aria-labelledby="tokens"] td[data-l="project"]')).map((td) => td.textContent));
    if (JSON.stringify(projects) !== JSON.stringify(["ai-software-factory-code", "ai-software-factory-code"])) fails.push(w + ": the token rows name their projects " + JSON.stringify(projects));
    if (w === 390) {
      const labels = await tp.page.evaluate(() => Array.from(document.querySelectorAll('[aria-labelledby="tokens"] tbody tr:first-child td[data-l]'))
        .map((td) => getComputedStyle(td, "::before").content));
      if (JSON.stringify(labels) !== JSON.stringify(['"project "', '"created "', '"expires "', '"last used "'])) fails.push("390: the stacked token row names its fields " + JSON.stringify(labels));
      const stack = await tp.page.evaluate(() => Array.from(document.querySelectorAll('[aria-labelledby="tokens"] tbody tr')).map((tr) => getComputedStyle(tr).display));
      if (stack.length !== 2 || stack.some((d) => d !== "block")) fails.push("390: the token rows do not stack: " + JSON.stringify(stack));
      const fold = await tp.page.evaluate(() => { const m = document.querySelector(".mint > summary"); return { display: getComputedStyle(m).display, h: m.getBoundingClientRect().height }; });
      if (fold.display !== "list-item" || fold.h < 43.5) fails.push("390: the token form's fold shows no marker or is under 44 px: " + JSON.stringify(fold));
    }
    await madeToken(tp.page);
    const made = await tp.page.evaluate(() => {
      const b = document.querySelector('[aria-labelledby="shown"]');
      return b && { top: b.getBoundingClientRect().top, text: b.innerText, line: document.getElementById("made-line").textContent,
        bearer: document.getElementById("made-bearer").textContent, copies: b.querySelectorAll("button[data-copy]").length };
    });
    if (!made) fails.push(w + ": the answer to the token form shows no shown-once block");
    else {
      if (made.top > vp.height - 120) fails.push(w + ": the shown-once block starts at " + Math.round(made.top) + " px, below the first screen");
      if (!/^sfc_[A-Za-z0-9_-]{43}$/.test(made.bearer)) fails.push(w + ": the block's bearer reads " + JSON.stringify(made.bearer));
      if (made.line !== 'claude mcp add --transport http factory-ai-software-factory-code https://console.example/mcp --header "Authorization: Bearer ' + made.bearer + '"')
        fails.push(w + ": the block's line reads " + JSON.stringify(made.line));
      if (!/shown once/i.test(made.text) || made.copies !== 2) fails.push(w + ": the block lacks its mark or its two Copy buttons");
      await tp.page.locator('button[data-copy="made-line"]').click();
      await tp.page.waitForTimeout(200);
      const clip = await tp.page.evaluate(() => navigator.clipboard.readText());
      const said = await tp.page.locator('button[data-copy="made-line"]').textContent();
      if (clip !== made.line || said !== "Copied") fails.push(w + ": Copy put " + JSON.stringify(clip.slice(0, 40)) + " on the clipboard and reads " + JSON.stringify(said));
      const live = await tp.page.locator('button[data-copy="made-line"]').getAttribute("aria-live");
      await tp.page.waitForTimeout(2200);
      const back = await tp.page.locator('button[data-copy="made-line"]').textContent();
      if (live !== "polite" || back !== "Copy") fails.push(w + ": Copy's label is live " + JSON.stringify(live) + " and reads " + JSON.stringify(back) + " after 2 s, want polite and Copy");
    }
    if (w === 390) {
      const small = await tp.page.evaluate(() => Array.from(document.querySelectorAll(".cline button, .mint > summary, .notify td.act button"))
        .filter((b) => b.offsetParent).map((b) => [b.textContent.trim().slice(0, 16), b.getBoundingClientRect().height]).filter((x) => x[1] < 43.5));
      if (small.length) fails.push("390: token targets under 44 px: " + small.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
    }
    const tsw = await sideways(tp.page);
    if (tsw) fails.push(w + ": horizontal scroll on the made token's page (" + tsw + " px)");
    await tp.ctx.close();
  }
  // a token named with all 64 characters the form allows scrolls no width
  // sideways: its name cuts at the desk and wraps on the phone
  await seed("tokens");
  {
    const lp = await open(browser, { width: 900, height: 900 });
    await lp.page.goto(base + "/notifications", { waitUntil: "load" });
    await lp.page.waitForTimeout(400);
    await madeToken(lp.page, "x".repeat(64));
    for (const width of [900, 960, 1090, 1440, 390]) {
      await lp.page.setViewportSize({ width, height: 900 });
      await lp.page.waitForTimeout(200);
      const lsw = await sideways(lp.page);
      if (lsw) fails.push(width + ": horizontal scroll on the Notifications page with a 64-character token name (" + lsw + " px)");
    }
    await lp.ctx.close();
  }
  // the contrast: every text pair of the C14 parts, both themes, 4.5:1 (3:1 for large text)
  for (const theme of ["dark", "light"]) {
    for (const [sc, act, sel, pg] of [["threads", "delete", contrastEl, ""], ["threads", "palette", contrastEl, ""], ["escalation", "inbox", contrastEl, ""], ["assisted", "level", contrastEl, ""],
      ["poke", "", contrastEl, ""], ["busy", "docs", contrastEl, ""], ["busy", "", ghostEl, ""], ["fire", "", ghostEl, ""], ["busy", "", notifyEl, "/notifications"],
      ["metrics", "", metricsEl, metricsPath], ["tokens", "", tokenEl, ""], ["tokens", "made", tokenEl, "/notifications"]]) {
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
      if (act === "inbox") await openInbox(cp.page);
      if (act === "made") await madeToken(cp.page);
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
  await criteriaWaive(browser, fails);
  await criteriaHuman(browser, fails);
  await browser.close();
  if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
  console.log("ok: every control present, the mechanics work");
})().catch((e) => { console.error(e.message || e); process.exit(1); });
