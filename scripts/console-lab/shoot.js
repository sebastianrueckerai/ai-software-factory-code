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
  const inflight = new Set(), failures = [], expected = [];
  page.on("request", (r) => { if (!r.url().includes("/events/stream")) inflight.add(r); });
  page.on("requestfinished", (r) => inflight.delete(r));
  page.on("requestfailed", (r) => { inflight.delete(r); if (!r.url().includes("/events/stream")) failures.push(r.url() + " " + r.failure().errorText); });
  page.on("response", (r) => { if (r.url().startsWith(base) && r.status() >= 400 && !expected.some((re) => re.test(r.url() + " " + r.status()))) failures.push(r.url() + " " + r.status()); });
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
  // expect marks an answer of 400 and above as the state a shot drives —
  // the refused clash save's 409 — so it is no failure of the page
  const expect = (re) => expected.push(re);
  return { ctx, page, idle, expect };
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
        // the C14 faces that open on a click: the level control's menu and its
        // confirm, the command palette, the delete warning
        if (s === "assisted") {
          const level = async (p) => {
            await p.page.locator("#level-btn").click();
            await p.idle();
          };
          await shot("level-1440.png", desk, "/", level, false);
          await shot("level-confirm-1440.png", desk, "/", async (p) => {
            await level(p);
            await p.page.locator('.lvlpop [data-lvl="autonomous"]').click();
            await p.idle();
          }, false);
          await shot("level-390.png", phone, "/", level, false);
        }
        if (s === "threads") {
          await shot("palette-1440.png", desk, "/", async (p) => {
            await p.page.locator("#chat-input").click();
            await p.page.keyboard.type("/");
            await p.idle();
          }, false);
          await shot("delete-1440.png", desk, "/", async (p) => {
            await p.page.locator("#thread-head [data-delete]").click();
            await p.idle();
          }, false);
        }
        // the routines (C16): the ghosts at the foot of Needs you and Queued, each
        // column's body scrolled to its foot; the routine's drawer, the control at
        // none, the held run, the phone at the Queued section's ghosts
        const feet = async (p) => {
          await p.page.evaluate(() => document.querySelectorAll("#board .column > .cards").forEach((b) => {
            if (b.querySelector(".card.ghost, .ghosts-hidden")) b.scrollTop = b.scrollHeight;
          }));
          await p.page.waitForTimeout(200);
        };
        if (s === "busy") await shot("routines-1440.png", desk, "/", feet, false);
        if (s === "busy" && theme === "dark") {
          await shot("routine-drawer-1440.png", desk, "/#routine=ai-software-factory-code/weekly-bump:overview", null, false);
          await shot("routines-hidden-1440.png", desk, "/?routines=none", feet, false);
          await shot("phone-ghosts-390.png", phone, "/", async (p) => {
            await p.page.locator("#board .column").filter({ has: p.page.locator("h3", { hasText: "Queued" }) }).locator(".card.ghost").first().scrollIntoViewIfNeeded();
            await p.page.waitForTimeout(200);
          }, false);
        }
        // the find as you type (2026-10-15): a word typed without Enter, the board
        // narrowed under the box that keeps it; the routines control open at within
        // 2 hours, the lane's hidden line under the ghosts, and so on the phone
        const ctl = async (p) => {
          await p.page.locator("#board .routines-ctl > summary").scrollIntoViewIfNeeded();
          await p.page.locator("#board .routines-ctl > summary").click();
          await p.page.waitForTimeout(200);
        };
        if (s === "busy") {
          await shot("find-typed-1440.png", desk, "/", async (p) => {
            await p.page.locator("#q").click();
            await p.page.keyboard.type("export", { delay: 40 });
            await p.page.waitForTimeout(400);
            await p.idle();
          }, false);
          await shot("routines-within-1440.png", desk, "/?routines=within&within=2&unit=hours", async (p) => {
            await feet(p);
            await ctl(p);
          }, false);
        }
        if (s === "busy" && theme === "dark") await shot("routines-within-390.png", phone, "/?routines=within&within=2&unit=hours", ctl, false);
        if (s === "fire" && theme === "dark") await shot("held-1440.png", desk, "/", feet, false);
        if (s !== "busy") continue;
        // the documents (roadmap 277): the roadmap read, a link into a
        // plan, Back, the hover peek, wide, and the phone
        const openDocs = async (p) => {
          await p.page.locator("#documents").click();
          await p.idle();
        };
        await shot("docs-roadmap-1440.png", desk, "/", openDocs, false);
        await shot("docs-link-1440.png", desk, "/", async (p) => {
          await openDocs(p);
          await p.page.locator('#doc-view a[data-doc="docs/plans/2026-10-15-documents-in-the-screen.md"]').click();
          await p.idle();
        }, false);
        await shot("docs-back-1440.png", desk, "/", async (p) => {
          await openDocs(p);
          await p.page.locator('#doc-view a[data-doc="docs/plans/2026-09-24-build-plan.md"]').click();
          await p.idle();
          await p.page.locator("#doc-back").click();
          await p.idle();
        }, false);
        await shot("docs-peek-1440.png", desk, "/", async (p) => {
          await openDocs(p);
          await p.page.locator('#doc-view a[data-doc="docs/plans/2026-09-24-build-plan.md"]').hover();
          await p.page.waitForTimeout(700);
        }, false);
        await shot("docs-wide-1440.png", desk, "/", async (p) => {
          await openDocs(p);
          await p.page.locator("#doc-wide").click();
          await p.idle();
        }, false);
        await shot("docs-390.png", phone, "/", openDocs, false);
        // the editor and its states (the 2026-10-15 plan, item 3): the
        // draft with its dot and its row, saving… on the slow save, the
        // landed commit, and the draft on the phone
        const openEditor = async (p) => {
          await openDocs(p);
          await p.page.locator("#doc-mode-edit").click();
          await p.idle();
          await p.page.keyboard.type("The draft in the editor, one keystroke at a time. ");
        };
        await shot("docs-edit-1440.png", desk, "/", async (p) => {
          await openEditor(p);
          await p.page.waitForTimeout(300);
        }, false);
        await shot("docs-saving-1440.png", desk, "/", async (p) => {
          await openEditor(p);
          await p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/docs-slow");
          await p.page.keyboard.press("Control+S");
          await p.page.waitForTimeout(700);
        }, false);
        await shot("docs-saved-1440.png", desk, "/", async (p) => {
          await openEditor(p);
          await p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/docs-slow?on=0");
          await p.page.keyboard.press("Control+S");
          await p.page.waitForFunction(() => document.querySelector("#doc-facts") && /Saved: \S{7} on master/.test(document.querySelector("#doc-facts").textContent), null, { timeout: 5000 });
        }, false);
        await shot("docs-draft-390.png", phone, "/", async (p) => {
          await openEditor(p);
          await p.page.waitForTimeout(300);
        }, false);
        // the outside change and the merge (items 5+6): the read view's
        // note with its reload, the editor's note saying the save will
        // merge, the landed merge, the refused clash with theirs beside
        // the editor, and load-theirs' ask — the editor's note on the
        // phone too
        const editorReady = (p) => p.page.waitForFunction(() => { const e = document.getElementById("doc-editor"); return e && !e.value.startsWith("Reading "); }, null, { timeout: 5000 });
        const noteSays = (p, re) => p.page.waitForFunction((r) => { const n = document.getElementById("doc-note"); const t = n && !n.hidden ? n.innerText.replace(/\s+/g, " ") : ""; return new RegExp(r).test(t); }, re.replace(/\s+/g, " ").trim(), { timeout: 5000 });
        const outside = (p, q) => p.page.evaluate(async (u) => { await fetch(u, { method: "POST" }); }, base + "/lab/docs-change" + (q || ""));
        await shot("docs-changed-read-1440.png", desk, "/", async (p) => {
          await openDocs(p);
          await outside(p);
          await noteSays(p, "changed by Sebastian \\d\\d:\\d\\d · reload");
        }, false);
        const changedEditor = async (p, q, typed) => {
          await openDocs(p);
          await p.page.locator("#doc-mode-edit").click();
          await editorReady(p);
          await p.page.keyboard.type(typed);
          await outside(p, q);
          await noteSays(p, "changed by Sebastian \\d\\d:\\d\\d · your save will merge · load theirs");
        };
        await shot("docs-changed-edit-1440.png", desk, "/", async (p) => {
          await changedEditor(p, "?line=3", "The changed editor shot's draft. ");
        }, false);
        await shot("docs-merged-1440.png", desk, "/", async (p) => {
          await changedEditor(p, "?line=3", "The merged shot's words. ");
          await p.page.keyboard.press("Control+S");
          await p.page.waitForFunction(() => { const f = document.querySelector("#doc-facts"); return f && /Saved: \S{7} on master · \d\d:\d\d · merged with Sebastian's change/.test(f.textContent); }, null, { timeout: 8000 });
          await p.idle();
        }, false);
        const clashEditor = async (p, typed) => {
          p.expect(/\/docs\/docs\/roadmap\.md 409$/);
          await changedEditor(p, "", typed);
          await p.page.keyboard.press("Control+S");
          await p.page.waitForFunction(() => document.querySelector("#doc-facts .doc-refused"), null, { timeout: 5000 });
        };
        await shot("docs-clash-1440.png", desk, "/", async (p) => {
          await clashEditor(p, "The clash shot's words. ");
          await p.page.locator('#doc-facts [data-act="theirs"]').click();
          await p.page.waitForTimeout(300);
        }, false);
        await shot("docs-load-theirs-1440.png", desk, "/", async (p) => {
          await clashEditor(p, "The load-theirs shot's words. ");
          await p.page.locator('#doc-facts [data-act="load-theirs"]').click();
          await p.page.waitForTimeout(300);
        }, false);
        await shot("docs-changed-edit-390.png", phone, "/", async (p) => {
          await changedEditor(p, "?line=3", "The phone's changed draft. ");
        }, false);
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
