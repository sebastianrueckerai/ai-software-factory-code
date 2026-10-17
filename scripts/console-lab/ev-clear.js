// ev-clear.js is the evidence shot of the chat's Clear old (the sweep):
// the inbox before and after, against the lab run.sh starts. Not part of
// the round's check; run it beside a lab:
//   CONSOLE_LAB_DIR=/tmp/x node scripts/console-lab/ev-clear.js
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");
const base = process.env.BASE || "http://127.0.0.1:8090";
const labDir = process.env.CONSOLE_LAB_DIR || "/tmp/claude-1000/console-lab";
const out = process.env.OUT || path.join(labDir, "ev");
const [name, value] = fs.readFileSync(path.join(labDir, "cookie.txt"), "utf8").trim().split(/=(.*)/s);
(async () => {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 });
  await ctx.addCookies([{ name, value, domain: new URL(base).hostname, path: "/", secure: true, httpOnly: true, sameSite: "Lax" }]);
  await ctx.addInitScript(() => localStorage.setItem("theme", "dark"));
  const p = await ctx.newPage();
  await fetch(base + "/lab/scenario/poke", { headers: { cookie: name + "=" + value } });
  await p.goto(base + "/", { waitUntil: "load" });
  await p.waitForTimeout(900);
  await p.locator("#thread-rows .trow").first().click();
  await p.waitForTimeout(900);
  await p.locator("#chat").screenshot({ path: out + "/clear-before.png" });
  await p.locator(".thead [data-clear]").click();
  await p.waitForTimeout(900);
  await p.locator("#chat").screenshot({ path: out + "/clear-after.png" });
  console.log("shots in " + out);
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
