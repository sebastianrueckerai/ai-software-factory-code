// check.js is the structural check of the console design round: against
// the lab (run.sh starts it) it reads every scenario in the dark theme
// and fails on a control of design/inventory.md that is missing, or a
// mechanic that does not work: the drawer docked at the right edge and
// never over the chat, Escape closing it, Do it becoming an applied line,
// the chat above the board at 390 px, no horizontal scroll at 1440 or 390.
// Exit 1 and one line per failure.
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");

const base = process.env.BASE || "http://127.0.0.1:8090";
const labDir = process.env.CONSOLE_LAB_DIR || "/tmp/claude-1000/console-lab";
const [name, value] = fs.readFileSync(path.join(labDir, "cookie.txt"), "utf8").trim().split(/=(.*)/s);

// contrastEl are the C14 parts' text, measured on the rendered page in
// both themes (design/rounds/02-multi-chat/check.js)
const contrastEl = ".trow .ttitle, .trow .tat, .tarch-l, .trow.arch .ttitle, .restore, .badge, .thint, .decider, .esc-l, .answer-l, .answer-or, " +
  ".cmd, .palette button span, .palette button b, #level-btn, .lvlpop b, .lvlpop .lw, .lvlpop .lp-head span, .lvl-confirm p, .msg.sum .said, " +
  ".msg.poke .who, .thead .ttl, .thr span, .tacts button, .overlay-box p, .overlay-box h2, .answer button, .arch-note, .thread-link";

// mustHave are the inventory's controls and data points as the lab's
// scenarios word them; a missing word is a missing control.
const mustHave = {
  threads: ["/compact", "/new", "mark · thread · last", "Why is the session store card waiting?", "Give the costs page a from and a to date.",
    "Plan the members page", "Export the project to GitHub", "a turn in flight", "1 unread", "New"],
  escalation: ["Answer:", "or in your words below", "needs you", "autonomous level · started by the agent", "Which store keeps the nightly board snapshots?"],
  poke: ["Inbox", "Why is the session store card waiting?", "1 unread", "archived", "restore", "Archive", "Delete…"],
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
    "project agent", "Start a new thread", "What is #606 stuck on", "Do it", "Skip", "Send", "own thread",
    "Wide view", "?", "Find on the board", "Find: a word, #612, plan:558",
    "Order and show", "Order", "Show", "newest first", "oldest first", "next up", "by stage", "by spend", "blocked only", "exceptional only", "failed only", "Apply"],
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

async function open(browser, viewport) {
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: 1 });
  await ctx.addCookies([{ name, value, domain: new URL(base).hostname, path: "/", secure: true, httpOnly: true, sameSite: "Lax" }]);
  await ctx.addInitScript(() => localStorage.setItem("theme", "dark"));
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  page.on("response", (r) => { if (r.url().startsWith(base) && r.status() >= 400) errors.push(r.url() + " " + r.status()); });
  return { ctx, page, errors };
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
      const theme0 = await p.page.evaluate(() => document.documentElement.dataset.theme);
      await p.page.locator("#theme-toggle").click();
      await p.page.waitForTimeout(200);
      if ((await p.page.evaluate(() => document.documentElement.dataset.theme)) === theme0) fails.push("busy: the theme toggle does not change data-theme");
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
    }
    if (sc === "busy") {
      // the find (C13): ?q= narrows every column, the counts read "n of m", the × clears
      await p.page.goto(base + "/?q=is%3Afailed", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const counts = await p.page.locator("#board .count").allInnerTexts();
      if (!counts.some((c) => /\d+ of \d+/.test(c))) fails.push("find: no count reads n of m under ?q=is:failed (" + counts.join(" | ") + ")");
      const faces = await p.page.locator("#board [data-card]").count(), failed = await p.page.locator("#board [data-card]").filter({ hasText: "failed" }).count();
      if (!faces || faces !== failed) fails.push("find: ?q=is:failed shows " + faces + " cards, " + failed + " of them failed");
      if (!(await p.page.locator("#find.on .find-x").count())) fails.push("find: no × while a find is on");
      if ((await p.page.locator("#board").getAttribute("data-query") || "").indexOf("q=is%3Afailed") < 0) fails.push("find: the board's query does not carry q");
      // the column menus (C13): ?done-only=failed narrows Done, its head says so with a way back
      await p.page.goto(base + "/?done-only=failed", { waitUntil: "load" });
      await p.page.waitForTimeout(600);
      const state = await p.page.locator("#board .col-state").allInnerTexts();
      if (!state.some((s) => s.includes("failed only"))) fails.push("menus: no state line reads failed only under ?done-only=failed (" + state.join(" | ") + ")");
      if (!(await p.page.locator("#board .col-state .col-x").count())) fails.push("menus: no × on the state line");
      if (!(await p.page.locator('#board input[name="done-only"][value="failed"]:checked').count())) fails.push("menus: the Done menu does not show failed only as chosen");
      if ((await p.page.locator("#board .col-menu").count()) !== 3) fails.push("menus: not three column menus");
    }
    if (sc === "threads") {
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
      // a poke on another thread: the inbox row first, its badge, and the sum in the tab title (C14)
      const first = p.page.locator("#thread-rows .trow").first();
      if (!(await first.evaluate((e) => e.classList.contains("inbox")))) fails.push("poke: the inbox is not the first row");
      if (!(await first.evaluate((e) => e.classList.contains("unread")))) fails.push("poke: the inbox row carries no unread badge");
      const title = await p.page.title();
      if (!title.startsWith("(2) ")) fails.push('poke: the tab title is "' + title + '", want the sum (2) first');
      await first.click();
      await p.page.waitForTimeout(600);
      const log = await p.page.locator("#chat").innerText();
      for (const w of ["#600 is done", "needs you", "Merge PR #612 of #606 waits for you"]) if (!log.includes(w)) fails.push('poke: the inbox shows no "' + w + '"');
      if ((await p.page.title()).startsWith("(2) ")) fails.push("poke: opening the inbox leaves its count in the tab title");
    }
    if (p.errors.length) fails.push(sc + ": " + p.errors[0]);
    await p.ctx.close();
  }
  // the phone: 44 px targets for the C14 controls, no horizontal scroll
  for (const sc of ["threads", "escalation"]) {
    await seed(sc);
    const pp = await open(browser, { width: 390, height: 844 });
    await pp.page.goto(base + "/", { waitUntil: "load" });
    await pp.page.waitForTimeout(600);
    const small = await pp.page.evaluate(() => Array.from(document.querySelectorAll(
      ".trow, .msg .choices button, .tacts button, .cmd, .restore, #level-btn, .open-arch, .swtg, .sw-sum, .answer button"))
      .filter((b) => b.offsetParent).map((b) => [b.className || b.id || b.tagName, b.getBoundingClientRect().height]).filter((x) => x[1] < 43.5));
    if (small.length) fails.push("phone: targets under 44 px in " + sc + ": " + small.map((x) => x[0] + " " + Math.round(x[1])).join(", "));
    const psw = await pp.page.evaluate(() => document.documentElement.scrollWidth);
    if (psw > 390) fails.push("phone: horizontal scroll at 390 in " + sc + " (" + psw + ")");
    await pp.ctx.close();
  }
  // the contrast: every text pair of the C14 parts, both themes, 4.5:1 (3:1 for large text)
  for (const theme of ["dark", "light"]) {
    for (const [sc, act] of [["threads", "delete"], ["escalation", "palette"], ["assisted", "level"], ["poke", ""]]) {
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
        for (const el of document.querySelectorAll(sel)) {
          if (!el.offsetParent || !el.textContent.trim()) continue;
          const cs = getComputedStyle(el), bg = bgOf(el), fg = over(rgb(cs.color), bg);
          const l1 = lum(fg), l2 = lum(bg), r = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
          const big = parseFloat(cs.fontSize) >= 24 || (parseFloat(cs.fontSize) >= 18.66 && +cs.fontWeight >= 700);
          if (r < (big ? 3 : 4.5)) out.push((el.className || el.tagName) + ' "' + el.textContent.trim().slice(0, 18) + '" ' + r.toFixed(2));
        }
        return out;
      }, contrastEl);
      if (low.length) fails.push(theme + " " + sc + ": under 4.5:1: " + low.join("; "));
      await cp.ctx.close();
    }
  }
  await seed("busy");
  const ph = await open(browser, { width: 390, height: 844 });
  await ph.page.goto(base + "/", { waitUntil: "load" });
  await ph.page.waitForTimeout(600);
  const sw = await ph.page.evaluate(() => document.documentElement.scrollWidth);
  if (sw > 390) fails.push("phone: horizontal scroll at 390 (" + sw + ")");
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
