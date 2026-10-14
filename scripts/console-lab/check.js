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

// mustHave are the inventory's controls and data points as the lab's
// scenarios word them; a missing word is a missing control.
const mustHave = {
  busy: ["Factory console", "Board", "New task", "Projects", "Resources", "Costs", "Policy", "Alerts", "Log out", "sebastian", "Theme",
    "waiting on you", "3 working", "3 need you", "€61.40", "€120.00", "GitHub", "Two cards need your decision", "Configuration version 4",
    "Needs you", "Queued", "In progress", "Done",
    "Merge", "PR #612", "high", "migration", "Approve", "Deny", "Split", "requesting changes", "rejecting",
    "Which store keeps the nightly board snapshots", "Postgres", "SQLite", "Ask the author", "answer on the issue", "Close",
    "How far back does the feed page by default?", "One day", "One week",
    "#598", "#616", "#617", "waiting on #603, #611", "#533",
    "#606", "3/4 checks", "€1.72", "near its budget", "#611", "#614", "#601", "merge queue", "PR #605",
    "#531", "#596", "#600", "#597", "failed", "#590", "closed", "#535", "Retry",
    "project agent", "New conversation", "What is #606 stuck on", "Do it", "Skip", "Send", "own conversation",
    "every accent", "Wide view", "?"],
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
    const low = (await text(p.page)).toLowerCase();
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
      const doit = p.page.locator("#chat").getByRole("button", { name: "Do it", exact: true });
      if (!(await doit.count())) fails.push("chat: no Do it");
      else {
        await doit.last().click();
        await p.page.waitForTimeout(800);
        const t2 = await p.page.locator("#chat").innerText();
        if (!/applied/i.test(t2) || !/undo/i.test(t2)) fails.push("chat: Do it does not become an applied line with Undo");
      }
    }
    if (p.errors.length) fails.push(sc + ": " + p.errors[0]);
    await p.ctx.close();
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
