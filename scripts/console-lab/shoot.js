// shoot.js screenshots the console lab (internal/console/lab_test.go,
// scripts/console-lab/run.sh starts it): every scenario in both themes,
// into $OUT/<scenario>/<theme>/. It fails on a page that answers other
// than 200, on any of the console's requests that fails, and on a cookie
// the console refuses.
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");

const base = process.env.BASE || "http://127.0.0.1:8090";
const labDir = process.env.CONSOLE_LAB_DIR || "/tmp/claude-1000/console-lab";
const out = process.env.OUT || path.join(labDir, "shots");
const scenarios = ["busy", "idle", "fire", "quiet", "assisted", "threads", "compacted", "poke", "escalation"];
const themes = ["dark", "light"];
const desk = { width: 1440, height: 900 };
const wide = { width: 1920, height: 1080 };
const tall = { width: 1440, height: 2600 };
const phone = { width: 390, height: 844 };
const pages = ["costs", "alerts", "resources", "projects", "policy"];

const [name, value] = fs.readFileSync(path.join(labDir, "cookie.txt"), "utf8").trim().split(/=(.*)/s);

// seed loads scenario s into the lab and answers the running card its
// drawer shots open, "" for none.
async function seed(s) {
  const resp = await fetch(base + "/lab/scenario/" + s);
  if (resp.status !== 204) throw new Error("GET /lab/scenario/" + s + ": " + resp.status + " " + (await resp.text()));
  return resp.headers.get("x-lab-drawer") || "";
}

// open is a fresh page of browser at viewport in theme, the session's
// cookie set; idle waits until none of its requests but the event stream,
// which stays open, has run for 500 ms, then 600 ms more; failures are
// the console's answers of 400 and above.
async function open(browser, viewport, theme) {
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: 1 });
  await ctx.addCookies([{ name, value, domain: new URL(base).hostname, path: "/", secure: true, httpOnly: true, sameSite: "Lax" }]);
  await ctx.addInitScript((t) => localStorage.setItem("theme", t), theme);
  const page = await ctx.newPage();
  const inflight = new Set(), failures = [];
  page.on("request", (r) => { if (!r.url().includes("/events/stream")) inflight.add(r); });
  page.on("requestfinished", (r) => inflight.delete(r));
  page.on("requestfailed", (r) => { inflight.delete(r); if (!r.url().includes("/events/stream")) failures.push(r.url() + " " + r.failure().errorText); });
  page.on("response", (r) => { if (r.url().startsWith(base) && r.status() >= 400) failures.push(r.url() + " " + r.status()); });
  page.on("pageerror", (e) => failures.push("script error: " + e.message));
  async function idle() {
    const start = Date.now();
    let quiet = Date.now();
    while (Date.now() - quiet < 500) {
      if (inflight.size) quiet = Date.now();
      if (Date.now() - start > 20000) throw new Error("never idle: " + [...inflight].map((r) => r.url()).join(", "));
      await page.waitForTimeout(50);
    }
    await page.waitForTimeout(600);
    if (failures.length) throw new Error(page.url() + ": " + failures.join("; "));
  }
  return { ctx, page, idle };
}

async function go(p, url) {
  const resp = await p.page.goto(base + url, { waitUntil: "load" });
  const at = new URL(p.page.url());
  if (at.origin !== base || at.pathname === "/login") throw new Error("GET " + url + ": the console refused the cookie, at " + p.page.url());
  if (!resp || resp.status() !== 200) throw new Error("GET " + url + ": " + (resp ? resp.status() : "no answer"));
  await p.idle();
}

// doIt presses the last Do it of a chat that offers more than one, so the
// shot shows an applied line with its Undo beside a proposal still open.
async function doIt(p) {
  const buttons = p.page.locator("#chat").getByRole("button", { name: "Do it", exact: true });
  if ((await buttons.count()) < 2) return;
  await buttons.last().click();
  await p.idle();
}

async function main() {
  const browser = await chromium.launch();
  let n = 0;
  try {
    for (const s of scenarios) {
      for (const theme of themes) {
        const dir = path.join(out, s, theme);
        fs.mkdirSync(dir, { recursive: true });
        // each shot seeds its scenario again: a Do it or a click changes the fakes
        const shot = async (file, viewport, url, act, fullPage = true) => {
          const drawer = await seed(s);
          const p = await open(browser, viewport, theme);
          try {
            await go(p, typeof url === "function" ? url(drawer) : url);
            if (act) await act(p);
            await p.page.screenshot({ path: path.join(dir, file), fullPage });
            n++;
            console.log(path.join(s, theme, file));
          } finally {
            await p.ctx.close();
          }
        };
        const drawer = await seed(s);
        await shot("board-1440.png", desk, "/", doIt);
        await shot("board-1920.png", wide, "/", doIt);
        // the columns scroll inside the viewport: a tall one shows every card
        await shot("board-tall-1440.png", tall, "/", doIt);
        await shot("wide-1440.png", desk, "/?view=wide");
        // the status in detail, opened on the strip's chevron
        await shot("status-open-1440.png", desk, "/", async (p) => {
          await p.page.locator("#status-toggle").click();
          await p.idle();
        }, false);
        if (drawer) {
          const tab = (title) => async (p) => {
            await p.page.locator("#drawer").getByRole("tab", { name: title, exact: true }).click();
            await p.idle();
          };
          await shot("drawer-running-1440.png", desk, (id) => "/#card=" + id, null, false);
          await shot("drawer-overview-1440.png", desk, (id) => "/#card=" + id, tab("Overview"), false);
          await shot("drawer-files-1440.png", desk, (id) => "/#card=" + id, tab("Files"), false);
          // the Overview's Time section open (C10)
          await shot("drawer-time-1440.png", desk, (id) => "/#card=" + id, async (p) => {
            await tab("Overview")(p);
            await p.page.locator('#drawer [data-sec="time"] > summary').click();
            await p.page.waitForTimeout(200);
          }, false);
        }
        // the phone is one column: the chat first, the board below it
        await shot("phone-390.png", phone, "/", doIt);
        if (s !== "busy") continue;
        for (const pg of pages) await shot(pg + "-1440.png", desk, "/" + pg);
        await shot("timeline-1440.png", desk, "/tasks/" + drawer + "/timeline");
        await shot("new-task-1440.png", desk, "/tasks/new");
      }
    }
  } finally {
    await browser.close();
  }
  console.log(n + " shots in " + out);
}

main().catch((e) => {
  console.error(e.message || e);
  process.exit(1);
});
