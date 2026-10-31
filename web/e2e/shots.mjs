// Captures every view for the design critique and reports browser errors.
// Usage: node e2e/shots.mjs [outDir]
import { chromium } from "playwright";
import { AxeBuilder } from "@axe-core/playwright";
import fs from "node:fs";

const base = process.env.CUBOID_URL ?? "http://localhost:8080";
const out = process.argv[2] ?? "../artifacts/screenshots";
fs.mkdirSync(out, { recursive: true });

const browser = await chromium.launch();
const problems = [];

async function page(width, height) {
  const ctx = await browser.newContext({ viewport: { width, height } });
  const p = await ctx.newPage();
  p.on("pageerror", (e) => problems.push(`page error: ${e.message}`));
  p.on("console", (m) => m.type() === "error" && problems.push(`console: ${m.text().slice(0, 200)}`));
  return p;
}

async function shot(p, name, full = false) {
  await p.screenshot({ path: `${out}/${name}.png`, fullPage: full });
  console.log("shot", name);
}

async function axe(p, name) {
  const r = await new AxeBuilder({ page: p }).analyze();
  for (const v of r.violations.filter((v) => ["serious", "critical"].includes(v.impact))) {
    problems.push(`axe ${name}: ${v.id} (${v.impact}) x${v.nodes.length}: ${v.nodes[0].html.slice(0, 120)}`);
  }
}

const views = [
  ["monitor", "/monitor", 1280, 720],
  ["monitor-1440", "/monitor", 1440, 900],
  ["store-42", "/t/42", 1280, 720],
  ["store-42-phone", "/t/42", 390, 844],
  ["company-7", "/c/7", 1280, 720],
  ["isolation", "/isolation", 1280, 720],
  ["bench", "/bench", 1280, 720],
];

for (const [name, path, w, h] of views) {
  const p = await page(w, h);
  await p.goto(base + path);
  await p.waitForTimeout(5000);
  await shot(p, name);
  await shot(p, `${name}-full`, true);
  await axe(p, name);
  const over = await p.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  if (over > 0) problems.push(`${name}: page overflows horizontally by ${over}px`);
  const fonts = await p.evaluate(() => [...document.fonts].filter((f) => f.status === "loaded").map((f) => f.family));
  if (!fonts.some((f) => f.includes("Atkinson"))) problems.push(`${name}: UI font not loaded (${fonts.join(", ")})`);
  if (name === "store-42") {
    const first = p.locator(".figure-v button").first();
    if (await first.count()) {
      await first.click();
      await p.waitForTimeout(500);
      await shot(p, "drawer");
      await axe(p, "drawer");
      await p.getByRole("button", { name: "Check against main database" }).click();
      await p.waitForTimeout(6000);
      await shot(p, "drawer-checked");
      console.log("drawer verdict:", await p.locator(".drawer .exact, .drawer .bad, .drawer .alarm").first().textContent().catch(() => "none"));
    } else {
      problems.push("store-42: no figures rendered");
    }
  }
  await p.context().close();
}

await browser.close();
console.log(problems.length ? `PROBLEMS (${problems.length}):\n${[...new Set(problems)].join("\n")}` : "no problems");
