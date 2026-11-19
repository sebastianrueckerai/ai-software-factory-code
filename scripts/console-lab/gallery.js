// gallery.js shoots the gallery of the console lab (GET /lab/gallery,
// internal/console/gallery_test.go): one PNG of each entry, its small card
// beside its opened card, at 1280 px and at 390 px, into $OUT, by default
// shots/gallery/<date>/ of the repository. `scripts/console-lab/run.sh
// gallery [out]` starts the lab and runs it. It fails on a gallery that
// does not answer 200 and, after its shots, on an entry that did not
// render or whose small card holds an element wider than the card at
// 390 px.
const fs = require("fs");
const path = require("path");
const { chromium } = require("playwright");

const base = process.env.BASE || "http://127.0.0.1:8090";
const out = process.env.OUT || path.join(__dirname, "..", "..", "shots", "gallery", new Date().toISOString().slice(0, 10));
const widths = [1280, 390];

async function main() {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch();
  let n = 0;
  const problems = new Set();
  try {
    for (const width of widths) {
      const page = await browser.newPage({ viewport: { width, height: 900 }, deviceScaleFactor: 1 });
      // the first read seats and renders every entry
      const resp = await page.goto(base + "/lab/gallery", { waitUntil: "load", timeout: 600000 });
      if (!resp || resp.status() !== 200) throw new Error("GET /lab/gallery: " + (resp ? resp.status() : "no answer"));
      const entries = page.locator("section.gallery-entry");
      const count = await entries.count();
      if (!count) throw new Error("the gallery holds no entry");
      for (let i = 0; i < count; i++) {
        const e = entries.nth(i);
        const problem = e.locator(":scope > .problem");
        if (await problem.count()) problems.add((await e.getAttribute("data-entry")) + ": " + (await problem.textContent()));
        await e.screenshot({ path: path.join(out, (await e.getAttribute("id")) + "-" + width + ".png") });
        n++;
        if (width === 390) {
          const wide = await e.locator(".gallery-small article.card").evaluateAll((cards) => cards.flatMap((card) => {
            const edge = card.getBoundingClientRect().right + 0.5;
            return [...card.querySelectorAll("*")].filter((el) => el.getBoundingClientRect().right > edge)
              .map((el) => el.tagName.toLowerCase() + (el.className ? "." + String(el.className).split(" ").join(".") : ""));
          }));
          if (wide.length) problems.add((await e.getAttribute("data-entry")) + ": wider than the card at 390 px: " + wide.join(", "));
        }
      }
      await page.close();
    }
  } finally {
    await browser.close();
  }
  console.log(n + " shots in " + out);
  if (problems.size) throw new Error([...problems].join("\n"));
}

main().catch((e) => {
  console.error(e.message || e);
  process.exit(1);
});
