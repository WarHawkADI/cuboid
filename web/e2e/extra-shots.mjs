// Views that need a click first: the SOC alerts switch and a finished leak drill.
import { chromium } from "playwright";

const base = process.env.CUBOID_URL ?? "http://localhost:8080";
const out = process.argv[2] ?? "../artifacts/screenshots";
const browser = await chromium.launch();
const p = await (await browser.newContext({ viewport: { width: 1280, height: 720 } })).newPage();
const errors = [];
p.on("pageerror", (e) => errors.push(e.message));

await p.goto(`${base}/t/42`);
await p.getByRole("button", { name: "SOC alerts" }).click();
await p.waitForTimeout(3000);
await p.screenshot({ path: `${out}/alerts-42-full.png`, fullPage: true });

await p.goto(`${base}/isolation`);
await p.waitForTimeout(1500);
await p.getByRole("button", { name: "Run leak drill" }).nth(1).click();
await p.waitForTimeout(12000);
await p.screenshot({ path: `${out}/isolation-drill-full.png`, fullPage: true });

await browser.close();
console.log(errors.length ? `PROBLEMS: ${errors.join("; ")}` : "no problems");
