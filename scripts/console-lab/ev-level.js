// ev-level.js is the evidence shot of the level control (C32): the lamp at
// each of the four levels and the open menu, in both themes, against the
// lab run.sh starts. Not part of the round's check; run it beside a lab:
//   CONSOLE_LAB_DIR=/tmp/x node scripts/console-lab/ev-level.js
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
  for (const theme of ["dark", "light"]) {
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 3 });
    await ctx.addCookies([{ name, value, domain: new URL(base).hostname, path: "/", secure: true, httpOnly: true, sameSite: "Lax" }]);
    await ctx.addInitScript((t) => localStorage.setItem("theme", t), theme);
    const p = await ctx.newPage();
    await fetch(base + "/lab/scenario/assisted", { headers: { cookie: name + "=" + value } });
    await p.goto(base + "/", { waitUntil: "load" });
    await p.waitForTimeout(900);
    const btn = p.locator("#level-btn"), pop = p.locator(".lvlpop");
    for (const level of ["manual", "assisted", "autonomous", "full auto"]) {
      await btn.click();
      if (level === "manual") await pop.screenshot({ path: out + "/level-menu-" + theme + ".png" });
      await pop.locator('[data-lvl="' + level + '"]').click();
      await p.waitForTimeout(700);
      // the lamp's halo reaches past the control's box: the shot takes a margin
      const box = await p.locator(".lvlctl").boundingBox();
      await p.screenshot({ path: out + "/level-" + level.replace(" ", "-") + "-" + theme + ".png",
        clip: { x: box.x - 16, y: box.y - 16, width: box.width + 32, height: box.height + 32 } });
    }
    await ctx.close();
  }
  console.log("shots in " + out);
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
