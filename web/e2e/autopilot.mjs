// Runs the demo Autopilot in a real browser, step by step, and records it.
// Usage: node e2e/autopilot.mjs [pauseSecondsPerBeat]
import { chromium } from "playwright";
import fs from "node:fs";

const base = process.env.CUBOID_URL ?? "http://localhost:8080";
const pause = Number(process.argv[2] ?? 5) * 1000;
const out = "../artifacts";
fs.mkdirSync(out, { recursive: true });

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1280, height: 720 }, recordVideo: { dir: `${out}/video`, size: { width: 1280, height: 720 } } });
const p = await ctx.newPage();
const errors = [];
p.on("pageerror", (e) => errors.push(e.message));
await p.goto(`${base}/monitor`);
await p.waitForTimeout(3000);
await p.getByRole("button", { name: "Run demo" }).click();

const bar = p.getByTestId("autopilot");
const start = Date.now();
let failed = 0;
for (let i = 0; i < 20; i++) {
  const next = bar.getByRole("button", { name: /^(Next|Finish)$/ });
  const label = await next.textContent();
  if (label === "Finish") break;
  const t0 = Date.now();
  await next.click();
  await p.waitForFunction((n) => {
    const el = document.querySelector('[data-testid="autopilot"]');
    return el && el.dataset.step === String(n) && el.dataset.busy === "false";
  }, i, { timeout: 180000 });
  const title = await bar.locator("strong").textContent();
  const status = await bar.getByRole("status").textContent();
  const bad = (await bar.getAttribute("data-failed")) === "true";
  if (bad) failed++;
  console.log(`${bad ? "FAIL" : "ok  "} ${((Date.now() - t0) / 1000).toFixed(1)}s  ${title}\n       ${status}`);
  await p.screenshot({ path: `${out}/screenshots/autopilot-${i + 1}.png` });
  await p.waitForTimeout(pause);
}
const total = (Date.now() - start) / 1000;
console.log(`Autopilot: ${failed} failed beats, ${total.toFixed(0)} s in total, browser errors: ${errors.length ? errors.join("; ") : "none"}`);
await p.close();
const video = await p.video().path();
await ctx.close();
await browser.close();
fs.copyFileSync(video, `${out}/demo_walkthrough.webm`);
console.log("recorded", `${out}/demo_walkthrough.webm`);
process.exit(failed ? 1 : 0);
