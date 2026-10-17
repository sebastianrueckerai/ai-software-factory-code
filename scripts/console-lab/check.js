// check.js is the structural check of the console design round: against
// the lab (run.sh starts it) it reads every scenario in the dark theme
// and fails on a control of design/inventory.md that is missing, or a
// mechanic that does not work: the drawer docked at the right edge and
// never over the chat, Escape closing it, Do it becoming an applied line,
// the routines' ghosts at the foot of their lanes with their control and
// drawer and their period, the find narrowing the board as you type, the
// chat above the board at 390 px, no horizontal scroll at 1440 or 390.
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
const contrastEl = ".trow .ttitle, .trow .tat, .trow.arch .ttitle, .tarch-l, .restore, .badge, .thint, .decider, .esc-l, .answer-l, .answer-or, " +
  ".cmd, .palette button span, .palette button b, #level-btn, .lvlpop b, .lvlpop .lw, .lvlpop .lp-head span, .lvl-confirm p, .msg.sum .said, " +
  ".msg.poke .who, .thead .ttl, .thr span, .tacts button, .overlay-box p, .overlay-box h2, .answer button, .arch-note, .thread-link, " +
  ".doc-path, .doc-facts, .doc-refused, .doc-dot, .doc-base, .doc-note, .doc-confirm-q, .doc-new-l, .doc-discard, #doc-new, " +
  ".doc-recent > summary, .doc-mode button, .doc-recent-label, .doc-none, #doc-article a, .doc-pins-l, .doc-pin, .doc-pin-add, #doc-forgejo";

// ghostEl are the routines' text (C16), measured as contrastEl is
const ghostEl = ".card.ghost .ref, .card.ghost .ttl, .card.ghost .line2, .card.ghost .held, .card.ghost .run, .card.ghost .acts button, " +
  ".chip.routine, .cell.routine";

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
  assisted: ["Inbox", "Board", "autonomy", "assisted", "Moved #8 to lane ready",
    "#7", "#8", "#9", "Speed up the find box", "Fix the login redirect", "Trim the roll notes"],

  busy: ["Factory console", "Board", "New task", "Projects", "Resources", "Costs", "Policy", "Alerts", "Log out", "sebastian", "Theme",
    "waiting on you", "3 working", "3 need you", "€61.40", "€120.00", "GitHub", "Two cards need your decision", "configuration", "version 4",
    "Needs you", "Queued", "In progress", "Done",
    "Merge", "PR #612", "high", "migration", "Approve", "Deny", "Split", "requesting changes", "rejecting",
    "Which store keeps the nightly board snapshots", "Postgres", "SQLite", "Ask the author", "answer on the issue", "Close",
    "How far back does the feed page by default?", "One day", "One week",
    "#598", "#616", "#617", "waiting on #603, #611", "#533",
    "#606", "3/4 checks", "€1.72", "near its budget", "#611", "#614", "#601", "PR #605",
    "#531", "#596", "#600", "#597", "failed", "#590", "closed", "#535", "Retry",
    "project agent", "Start a new thread", "What is #606 stuck on", "Do it", "Skip", "Send",
    "Wide view", "?", "Find on the board", "Find: a word, #612, plan:558",
    "Order and show", "Order", "Show", "newest first", "oldest first", "next up", "by stage", "by spend", "blocked only", "exceptional only", "failed only",
    "↻ routines", "within", "Weekly bump", "Template pass", "Dependency update", "Audit", "every Monday 06:00", "after 6 more merges",
    "Skip once", "Run now", "#779"],
  idle: ["idle", "0 working", "0 need you", "€144.16", "Factory is idle", "#455", "#178", "Done", "#618", "#597", "failed"],
  fire: ["#610", "Grant one session with this answer", "Fund", "real problem", "nothing for over", "2 waiting on the provider",
    "#620", "€1.95", "#615", "#621", "#604", "#609", "failed", "Resource request", "cx33", "load test of the event stream",
    "beyond the envelope", "#623", "waiting on #610", "#602"],
  quiet: ["One card is working", "1 working", "0 need you", "€4.10", "#624", "#622", "#621", "#620", "done"],
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
  await page.locator("#status-toggle").click();
  await page.waitForTimeout(200);
  if (!(await page.locator("#status-panel").isVisible())) fails.push(label + ": the toggle does not open the panel");
  await page.evaluate((card) => document.dispatchEvent(new CustomEvent("view",
    {detail: {query: "?project=ai-software-factory-code", card: card, tab: "chat", panel: "closed", theme: "light"}})), labID(616));
  await page.waitForTimeout(900);
  const card = new URL(page.url()).hash;
  if (card.indexOf("#card=") !== 0) fails.push(label + ": the drawer's hash is " + card);
  if (!(await page.locator("#drawer").isVisible())) fails.push(label + ": the drawer does not show");
  if (!(await page.locator('#drawer .tabs button[aria-selected="true"][hx-get*="tab=chat"]').first().isVisible())) fails.push(label + ": the tab does not land");
  if (!(await page.locator("#status-panel").isHidden())) fails.push(label + ": the panel does not fold");
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

async function text(page) {
  // what a disclosure hides (the deny form, the own-answer box) counts once opened
  await page.evaluate(() => document.querySelectorAll('button.deny[aria-expanded="false"], button.own-toggle[aria-expanded="false"]').forEach((b) => b.click()));
  await page.evaluate(() => document.querySelectorAll("details.col-menu").forEach((d) => { d.open = true; }));
  return page.evaluate(() => document.body.innerText + " " + Array.from(document.querySelectorAll("[aria-label],[title],[placeholder]"))
    .map((e) => (e.getAttribute("aria-label") || "") + " " + (e.getAttribute("title") || "") + " " + (e.getAttribute("placeholder") || "")).join(" "));
}

(async () => {
  const browser = await chromium.launch();
  const fails = [];
  for (const [sc, words] of Object.entries(mustHave)) {
    const drawer = await seed(sc);
    const p = await open(browser, { width: 1440, height: 900 });
    await p.page.goto(base + "/", { waitUntil: "load" });
    await p.page.waitForTimeout(600);
    // the status in detail opens on its chevron: its words count too
    const xp = p.page.locator("#status-toggle");
    if (await xp.count()) { await xp.click(); await p.page.waitForTimeout(200); }
    const low = (await text(p.page)).toLowerCase();
    if (await xp.count()) { await p.page.keyboard.press("Escape"); await p.page.waitForTimeout(200); }
    for (const w of words) if (!low.includes(w.toLowerCase())) fails.push(sc + ': missing "' + w + '"');
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
      const sw = await p.page.evaluate(() => document.documentElement.scrollWidth);
      if (sw > 1440) fails.push("busy: horizontal page scroll at 1440 (" + sw + ")");
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
          for (const w of ["Overview", "Chat", "Activity", "Files"]) if (!dt.includes(w)) fails.push('drawer: missing the tab "' + w + '"');
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
      const doit = p.page.locator("#chat").getByRole("button", { name: "Do it", exact: true });
      if (!(await doit.count())) fails.push("chat: no Do it");
      else {
        await doit.last().click();
        await p.page.waitForTimeout(800);
        const t2 = await p.page.locator("#chat").innerText();
        if (!/applied/i.test(t2) || !/undo/i.test(t2)) fails.push("chat: Do it does not become an applied line with Undo");
      }
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
    }
    if (sc === "busy") {
      // the routines (C16): a ghost at the foot of Needs you and of Queued, the control in the
      // board's head, the strip's cell, the chip, the routine's drawer, Skip once
      await seed("busy");
      await p.page.goto(base + "/", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const feet = await p.page.evaluate(() => Array.from(document.querySelectorAll("#board .column")).map((c) => {
        const kids = Array.from(c.querySelectorAll(":scope > .cards > article.card")), ghost = (k) => k.classList.contains("ghost");
        return { title: c.querySelector("h3").firstChild.textContent.trim(), first: kids.findIndex(ghost), last: kids.map((k) => !ghost(k)).lastIndexOf(true) };
      }));
      for (const t of ["Needs you", "Queued"]) {
        const f = feet.find((x) => x.title === t);
        if (!f || f.first < 0) fails.push("routines: no ghost in " + t);
        else if (f.first < f.last) fails.push("routines: a ghost of " + t + " sits above its last card");
      }
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
      // dependency update and 4 days none; all and none send no period, and a number
      // changed under none chooses within
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
        ["choosing all", () => ctl.locator("input[name=routines][value=all]").check(), { routines: "all" }, all, ""],
        ["choosing none", () => ctl.locator("input[name=routines][value=none]").check(), { routines: "none" }, "", "1 routine hidden | 3 routines hidden"],
        ["a number of 3 under none", async () => { await ctl.locator("input[name=within]").fill("3"); await ctl.locator("input[name=within]").press("Tab"); },
          { routines: "within", within: "3", unit: "days" }, soon, "1 routine hidden"],
      ]) {
        await ctl.locator("summary").click();
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
        if (!(await dlg.isVisible()) || !(await dlg.innerText()).includes("go for good")) fails.push("threads: Delete… opens no warning");
        else {
          await dlg.getByRole("button", { name: "Keep it" }).click();
          if (await dlg.count()) fails.push("threads: Keep it does not close the warning");
        }
      }
      await p.page.locator("#thread-head [data-archive]").click();
      await p.page.waitForTimeout(600);
      const restore = p.page.locator("#thread-rows .tarch [data-restore]");
      if (!(await restore.count())) fails.push("threads: Archive puts no row under the archived rule");
      else {
        // an archived thread opened: its composer is one line with restore (C14)
        await p.page.locator("#thread-rows .tarch [data-thread]").first().click();
        await p.page.waitForTimeout(600);
        if (await p.page.locator("#chat-input").isVisible()) fails.push("threads: an archived thread shows the text box");
        if (!(await p.page.locator(".arch-note").isVisible())) fails.push("threads: an archived thread's composer has no restore line");
        await restore.first().click();
        await p.page.waitForTimeout(600);
        if (await p.page.locator("#thread-rows .tarch").count()) fails.push("threads: restore leaves the thread archived");
      }
    }
    if (sc === "assisted") {
      // the level control under Send (C14): the four states with their words; another one
      // turns the menu into the confirm in place, Keep keeps the level, Escape closes the menu
      const btn = p.page.locator("#level-btn"), pop = p.page.locator(".lvlpop");
      if ((await btn.getAttribute("data-level")) !== "assisted") fails.push("level: the control does not show assisted");
      await btn.click();
      if ((await pop.locator("[data-lvl]").count()) !== 4 || (await pop.locator(".lw").count()) !== 4) fails.push("level: the menu does not list the four states with their words");
      if ((await pop.locator('[aria-checked="true"]').getAttribute("data-lvl")) !== "assisted") fails.push("level: the menu does not check the current state");
      await pop.locator('[data-lvl="autonomous"]').click();
      const conf = await pop.innerText();
      for (const w of ["autonomy to autonomous?", "policy change", "audit", "Set", "Keep"]) if (!conf.includes(w)) fails.push('level: the confirm lacks "' + w + '"');
      await pop.getByRole("button", { name: "Keep" }).click();
      if (await pop.isVisible()) fails.push("level: Keep does not close the menu");
      if ((await btn.getAttribute("data-level")) !== "assisted") fails.push("level: Keep changed the level");
      await btn.click();
      await p.page.keyboard.press("Escape");
      if (await pop.isVisible()) fails.push("level: Escape does not close the menu");
      if (await p.page.locator("form#autonomy").count()) fails.push("level: the board head still carries the old selector");
    }
    if (sc === "escalation") {
      // the answer row (C14): the pending escalation's choices once, between the log and the box;
      // a choice answers its card with no model turn and the row goes
      const row = p.page.locator("#chat-answer");
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
      // a poke on another thread: the inbox row first, its split badge, and the split in the tab title (C14, the settle concept)
      const first = p.page.locator("#thread-rows .trow").first();
      if (!(await first.evaluate((e) => e.classList.contains("inbox")))) fails.push("poke: the inbox is not the first row");
      if (!(await first.evaluate((e) => e.classList.contains("unread")))) fails.push("poke: the inbox row carries no unread badge");
      const badge = first.locator(".badge");
      if (!(await badge.count()) || (await badge.getAttribute("aria-label")) !== "1 needs you · 1 new")
        fails.push('poke: the inbox row carries no split badge "1 needs you · 1 new"');
      const title = await p.page.title();
      if (!title.startsWith("(1 needs you · 2 new) ")) fails.push('poke: the tab title is "' + title + '", want the split (1 needs you · 2 new) first');
      await first.click();
      await p.page.waitForTimeout(600);
      const log = await p.page.locator("#chat").innerText();
      for (const w of ["#600 is done", "needs you", "Merge PR #612 of #606 waits for you", "moved on ·"]) if (!log.includes(w)) fails.push('poke: the inbox shows no "' + w + '"');
      const settled = p.page.locator("#chat-log .msg.esc.settled");
      if (!(await settled.count())) fails.push("poke: the answered merge ask does not sit settled");
      else if (await settled.locator("button").count()) fails.push("poke: the settled ask keeps its buttons");
      if ((await p.page.title()).includes("2 new")) fails.push("poke: opening the inbox leaves its count in the tab title");
    }
    if (p.errors.length) fails.push(sc + ": " + p.errors[0]);
    await p.ctx.close();
  }
  // the phone: 44 px targets for the C14 controls and the theme selector, no horizontal scroll
  for (const sc of ["threads", "escalation"]) {
    await seed(sc);
    const pp = await open(browser, { width: 390, height: 844 });
    await pp.page.goto(base + "/", { waitUntil: "load" });
    await pp.page.waitForTimeout(600);
    const small = await pp.page.evaluate(() => Array.from(document.querySelectorAll(
      ".trow, .msg .choices button, .tacts button, .cmd, .restore, #level-btn, .open-arch, .swtg, .sw-sum, .answer button, .theme-switch button"))
      .filter((b) => b.offsetParent).map((b) => [b.className || b.id || b.tagName, b.getBoundingClientRect().height]).filter((x) => x[1] < 43.5));
    if (small.length) fails.push("phone: targets under 44 px in " + sc + ": " + small.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
    const psw = await pp.page.evaluate(() => document.documentElement.scrollWidth);
    if (psw > 390) fails.push("phone: horizontal scroll at 390 in " + sc + " (" + psw + ")");
    await pp.ctx.close();
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
      const psw = await pp.page.evaluate(() => document.documentElement.scrollWidth);
      if (psw > 390) fails.push("phone: horizontal scroll at 390 with the documents (" + psw + ")");
      await pp.page.keyboard.press("Escape");
      await pp.page.waitForTimeout(300);
      if (await pp.page.locator("#doc-view").isVisible()) fails.push("phone: Escape does not close the documents");
    }
    await pp.ctx.close();
  }
  // the contrast: every text pair of the C14 parts, both themes, 4.5:1 (3:1 for large text)
  for (const theme of ["dark", "light"]) {
    for (const [sc, act, sel] of [["threads", "delete", contrastEl], ["escalation", "palette", contrastEl], ["assisted", "level", contrastEl],
      ["poke", "", contrastEl], ["busy", "docs", contrastEl], ["busy", "", ghostEl], ["fire", "", ghostEl]]) {
      await seed(sc);
      const cp = await open(browser, { width: 1440, height: 900 });
      // after open's own init script, so this theme wins
      await cp.ctx.addInitScript((t) => localStorage.setItem("theme", t), theme);
      await cp.page.goto(base + "/", { waitUntil: "load" });
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
  const sw = await ph.page.evaluate(() => document.documentElement.scrollWidth);
  if (sw > 390) fails.push("phone: horizontal scroll at 390 (" + sw + ")");
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
  await browser.close();
  if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
  console.log("ok: every control present, the mechanics work");
})().catch((e) => { console.error(e.message || e); process.exit(1); });
